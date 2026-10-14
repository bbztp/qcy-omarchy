package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/qcycore"
	"tinygo.org/x/bluetooth"
)

// charKind names the readable characteristics the daemon uses besides the
// command/notify pair.
type charKind int

const (
	charBattery charKind = iota
	charVersion
	charKeys
)

// session is one open GATT link to the earbuds. The daemon only talks to this
// interface, which is what lets the lifecycle be tested without hardware.
type session interface {
	// WriteCmd writes one command frame (without-response first, with-response
	// as fallback).
	WriteCmd(pkt []byte) error
	// Has reports whether the characteristic exists on this device.
	Has(k charKind) bool
	Read(k charKind, buf []byte) (int, error)
	WriteKeys(raw []byte) error
	// Close disables notifications and disconnects, releasing the adapter.
	Close()
}

// dialer opens a session. onNotify receives every raw notification payload
// from the command-notify characteristic and must not be called after Close.
type dialer interface {
	Dial(onNotify func([]byte)) (s session, deviceName string, err error)
}

// bleDialer is the real implementation on top of BlueZ.
type bleDialer struct {
	scanSeconds int
	gate        dialGate

	// vendor is the manufacturer-data vendorId of the unit the last successful
	// Dial reached (from the scan, or from the cache on the fast path).
	vmu        sync.Mutex
	vendor     uint16
	haveVendor bool
	// color is the colorIndex the earbuds advertised (from the scan, or from
	// the cache on the fast path: a connected unit stops advertising).
	color     uint8
	haveColor bool
	// colorTries / lastColorTry bound the short scans that look for the colour
	// when the cache has a vendorId but no colour (per daemon lifetime).
	colorTries   int
	lastColorTry time.Time
}

const (
	colorProbeTimeout  = 3 * time.Second
	colorProbeMaxTries = 3
	colorProbeInterval = 10 * time.Minute
)

// advertScan looks for the advertisement of addr for up to timeout and returns
// its vendorId and colorIndex. It is a variable so tests can fake it.
var advertScan = func(adapter *bluetooth.Adapter, addr bluetooth.Address, timeout time.Duration) (uint16, uint8, bool) {
	type res struct {
		v uint16
		c uint8
	}
	ch := make(chan res, 1)
	go func() {
		_ = adapter.Scan(func(_ *bluetooth.Adapter, r bluetooth.ScanResult) {
			if r.Address.String() != addr.String() {
				return
			}
			for _, m := range r.ManufacturerData() {
				if v, c, ok := qcycore.ParseAdvert(m.CompanyID, m.Data); ok {
					select {
					case ch <- res{v, c}:
					default:
					}
					return
				}
			}
		})
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	var out res
	found := false
	select {
	case out = <-ch:
		found = true
	case <-t.C:
	}
	_ = adapter.StopScan()
	return out.v, out.c, found
}

// probeColor makes one short scan for the colour when the cache knows the
// vendorId but not the colour (an entry written before the colour was kept).
// It is bounded: at most colorProbeMaxTries per daemon lifetime, one per
// colorProbeInterval. A unit held by another client does not advertise, so a
// miss simply falls back to the plain fast path.
func (b *bleDialer) probeColor(adapter *bluetooth.Adapter, addr bluetooth.Address, ce cacheEntry) (uint8, bool) {
	if !ce.HaveVendor || ce.HaveColor {
		return ce.ColorIndex, ce.HaveColor
	}
	b.vmu.Lock()
	if b.colorTries >= colorProbeMaxTries || (!b.lastColorTry.IsZero() && time.Since(b.lastColorTry) < colorProbeInterval) {
		b.vmu.Unlock()
		return 0, false
	}
	b.colorTries++
	b.lastColorTry = time.Now()
	b.vmu.Unlock()
	v, c, ok := advertScan(adapter, addr, colorProbeTimeout)
	if !ok || v != ce.VendorID {
		return 0, false
	}
	return c, true
}

func (b *bleDialer) setVendor(v uint16, ok bool) {
	b.vmu.Lock()
	b.vendor, b.haveVendor = v, ok
	b.vmu.Unlock()
}

func (b *bleDialer) setColor(c uint8, ok bool) {
	b.vmu.Lock()
	b.color, b.haveColor = c, ok
	b.vmu.Unlock()
}

// ColorIndex implements colorSource.
func (b *bleDialer) ColorIndex() (uint8, bool) {
	b.vmu.Lock()
	defer b.vmu.Unlock()
	return b.color, b.haveColor
}

// VendorID implements vendorSource.
func (b *bleDialer) VendorID() (uint16, bool) {
	b.vmu.Lock()
	defer b.vmu.Unlock()
	return b.vendor, b.haveVendor
}

// vendorSource is the optional dialer capability the daemon uses to learn the
// vendorId of the unit it just connected to. Dialers without it (test fakes)
// simply leave detection to the advertised name.
type vendorSource interface {
	VendorID() (uint16, bool)
}

// colorSource is the optional dialer capability that reports the advertised
// colorIndex of the unit it connected to.
type colorSource interface {
	ColorIndex() (uint8, bool)
}

// hardwareAllowed is the structural guard against tests reaching the real
// earbuds: TestMain sets it to false and the real dialer refuses first.
var hardwareAllowed = true

var errHardwareDisabled = errors.New("hardware access disabled in tests")

func (b *bleDialer) Dial(onNotify func([]byte)) (session, string, error) {
	if !hardwareAllowed {
		return nil, "", errHardwareDisabled
	}
	b.setVendor(0, false)
	b.setColor(0, false)
	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		return nil, "", fmt.Errorf("enable adapter: %w", err)
	}

	// Fast path: cached address. A failure that looks like "someone else holds
	// the device" is reported as busy right away: scanning would not find a
	// device that is connected elsewhere (it stops advertising) and would only
	// burn the whole scan window.
	if addr, ok := loadCachedAddr(); ok {
		name := loadCachedName()
		ce, _ := loadCacheEntry()
		vendor, haveVendor := ce.VendorID, ce.HaveVendor
		color, haveColor := b.probeColor(adapter, addr, ce)
		dev, err := b.dialLE(adapter, addr, 3*time.Second)
		if err == nil {
			s, serr := setupSession(dev, onNotify)
			if serr == nil {
				saveCachedAddr(addr, name, vendor, haveVendor, color, haveColor)
				b.setVendor(vendor, haveVendor)
				b.setColor(color, haveColor)
				return s, name, nil
			}
			_ = dev.Disconnect()
			err = serr
		}
		if looksBusy(err) {
			return nil, "", &busyError{cause: err}
		}
	}

	// Scan path
	// The scan reads the raw manufacturer data (one scan, no second client):
	// the vendorId and the colorIndex are both in it, and a connected unit
	// stops advertising, so this is the only moment they are visible.
	addrCh := make(chan bluetooth.Address, 1)
	nameCh := make(chan string, 1)
	scanErrCh := make(chan error, 1)
	var vmu sync.Mutex
	vendors := map[string]uint16{}
	colors := map[string]uint8{}
	go func() {
		scanErrCh <- adapter.Scan(func(_ *bluetooth.Adapter, r bluetooth.ScanResult) {
			for _, m := range r.ManufacturerData() {
				v, c, ok := qcycore.ParseAdvert(m.CompanyID, m.Data)
				if !ok {
					continue
				}
				vmu.Lock()
				vendors[r.Address.String()] = v
				colors[r.Address.String()] = c
				vmu.Unlock()
				select {
				case addrCh <- r.Address:
				default:
				}
				if n := r.LocalName(); n != "" {
					select {
					case nameCh <- n:
					default:
					}
				}
				return
			}
		})
	}()

	var addr bluetooth.Address
	found := false
	timer := time.NewTimer(time.Duration(b.scanSeconds) * time.Second)
	defer timer.Stop()
	select {
	case addr = <-addrCh:
		found = true
	case err := <-scanErrCh:
		return nil, "", fmt.Errorf("scan: %w", err)
	case <-timer.C:
	}
	if !found {
		_ = adapter.StopScan()
		return nil, "", errNotFound
	}

	var dev bluetooth.Device
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		dev, err = b.dialLE(adapter, addr, scanConnectTimeout)
		if err == nil || errors.Is(err, errConnectTimeout) {
			// A timeout leaves the Connect pending in the gate; asking again
			// would only join it and wait for the same call.
			break
		}
		select {
		case addr = <-addrCh:
		default:
		}
		time.Sleep(500 * time.Millisecond)
	}
	_ = adapter.StopScan()
	if err != nil {
		return nil, "", fmt.Errorf("connect LE %s: %w", addr, err)
	}

	name := ""
	select {
	case name = <-nameCh:
	default:
	}
	s, err := setupSession(dev, onNotify)
	if err != nil {
		_ = dev.Disconnect()
		return nil, "", err
	}
	vmu.Lock()
	vendor, haveVendor := vendors[addr.String()]
	color, haveColor := colors[addr.String()]
	vmu.Unlock()
	haveColor = haveColor && haveVendor
	saveCachedAddr(addr, name, vendor, haveVendor, color, haveColor)
	b.setVendor(vendor, haveVendor)
	b.setColor(color, haveColor)
	return s, name, nil
}

const (
	scanConnectTimeout = 10 * time.Second
	pendingValidity    = 30 * time.Second
)

var errConnectTimeout = errors.New("connect timed out")

func (b *bleDialer) dialLE(adapter *bluetooth.Adapter, addr bluetooth.Address, timeout time.Duration) (bluetooth.Device, error) {
	return b.gate.get(
		func() (bluetooth.Device, error) { return adapter.Connect(addr, bluetooth.ConnectionParams{}) },
		func(dev bluetooth.Device) { _ = dev.Disconnect() },
		timeout,
	)
}

// pendingDial is one Connect call in flight. There is at most one per dialer:
// BlueZ shares one device object per address, so a second Connect (or a late
// Disconnect from an abandoned one) would act on the link another caller is
// about to use. Callers that need a dial while one is pending join it.
//
// waiters, done, dev and err are guarded by mu. Registering as a waiter and
// reading done happen under the same mu, so the completing goroutine's
// "does anyone want this device" decision cannot race a late joiner.
type pendingDial struct {
	mu      sync.Mutex
	done    bool
	waiters int
	dev     bluetooth.Device
	err     error
	ch      chan struct{} // closed once done
	started time.Time
}

// dialGate hands out the pending dial. Lock order: gate.mu then pending.mu.
type dialGate struct {
	mu       sync.Mutex
	cur      *pendingDial
	disc     chan struct{}    // non-nil while an orphan's Disconnect runs; closed when it ends
	now      func() time.Time // nil = time.Now
	validity time.Duration    // 0 = pendingValidity
}

func (g *dialGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// get returns the device of the pending Connect, starting one if none is
// valid. A caller that times out stops waiting; the Connect keeps going and is
// adopted by whoever joins it, or disconnected when it completes with nobody
// interested. A result adopted by several concurrent waiters is shared.
func (g *dialGate) get(connect func() (bluetooth.Device, error), disconnect func(bluetooth.Device), timeout time.Duration) (bluetooth.Device, error) {
	validity := g.validity
	if validity == 0 {
		validity = pendingValidity
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var p *pendingDial
	for p == nil {
		g.mu.Lock()
		if disc := g.disc; disc != nil {
			// An orphan's Disconnect is in flight. A new Connect now would act
			// on the device object that call is tearing down, so wait for it,
			// within the caller's own timeout and without holding g.mu.
			g.mu.Unlock()
			select {
			case <-disc:
				continue
			case <-timer.C:
				return bluetooth.Device{}, errConnectTimeout
			}
		}
		if c := g.cur; c != nil && g.clock().Sub(c.started) <= validity {
			c.mu.Lock()
			if !c.done {
				c.waiters++
				p = c
			}
			c.mu.Unlock()
		}
		if p == nil {
			p = &pendingDial{waiters: 1, ch: make(chan struct{}), started: g.clock()}
			g.cur = p
			go g.run(p, connect, disconnect)
		}
		g.mu.Unlock()
	}

	select {
	case <-p.ch:
	case <-timer.C:
		p.mu.Lock()
		if !p.done {
			p.waiters--
			p.mu.Unlock()
			return bluetooth.Device{}, errConnectTimeout
		}
		p.mu.Unlock() // completed at the deadline: adopt the result
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dev, p.err
}

func (g *dialGate) run(p *pendingDial, connect func() (bluetooth.Device, error), disconnect func(bluetooth.Device)) {
	dev, err := connect()
	g.mu.Lock()
	p.mu.Lock()
	p.dev, p.err = dev, err
	p.done = true
	adopt := p.waiters > 0
	close(p.ch)
	p.mu.Unlock()
	current := g.cur == p
	if current {
		g.cur = nil
	}
	// A pending replaced after its validity is ignored without disconnecting:
	// its device object may be the one a newer attempt now uses. The orphan's
	// Disconnect runs with g.mu released (it is an external call that can
	// hang); g.disc keeps new Connects from starting until it returns.
	if err != nil || adopt || !current {
		g.mu.Unlock()
		return
	}
	disc := make(chan struct{})
	g.disc = disc
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.disc = nil
		g.mu.Unlock()
		close(disc)
	}()
	disconnect(dev)
}

type bleSession struct {
	dev        bluetooth.Device
	cmdChar    bluetooth.DeviceCharacteristic
	notifyChar bluetooth.DeviceCharacteristic
	chars      map[charKind]*bluetooth.DeviceCharacteristic
}

func setupSession(dev bluetooth.Device, onNotify func([]byte)) (session, error) {
	services, err := dev.DiscoverServices([]bluetooth.UUID{svcUUIDParsed})
	if err != nil {
		return nil, fmt.Errorf("discover service a001: %w", err)
	}
	if len(services) == 0 {
		return nil, errors.New("QCY service a001 not found on LE device")
	}
	svc := services[0]

	req, err := svc.DiscoverCharacteristics([]bluetooth.UUID{cmdUUIDParsed, notifyUUIDParsed})
	if err != nil {
		return nil, fmt.Errorf("discover characteristics: %w", err)
	}
	if len(req) < 2 {
		return nil, errors.New("missing command/notify characteristics")
	}
	s := &bleSession{dev: dev, cmdChar: req[0], notifyChar: req[1], chars: map[charKind]*bluetooth.DeviceCharacteristic{}}
	if c := findChar(svc, battUUIDParsed); c != nil {
		s.chars[charBattery] = c
	}
	if c := findChar(svc, verUUIDParsed); c != nil {
		s.chars[charVersion] = c
	}
	if c := findChar(svc, keysUUIDParsed); c != nil {
		s.chars[charKeys] = c
	}
	probeCharacteristics(svc)

	if err := s.notifyChar.EnableNotifications(func(buf []byte) {
		// The callback must never wedge the BLE stack.
		defer func() { _ = recover() }()
		onNotify(append([]byte(nil), buf...))
	}); err != nil {
		return nil, fmt.Errorf("enable notifications 1002: %w", err)
	}
	return s, nil
}

func (s *bleSession) WriteCmd(pkt []byte) error {
	if _, err := s.cmdChar.WriteWithoutResponse(pkt); err == nil {
		return nil
	}
	_, err := s.cmdChar.Write(pkt)
	return err
}

func (s *bleSession) Has(k charKind) bool { return s.chars[k] != nil }

func (s *bleSession) Read(k charKind, buf []byte) (int, error) {
	c := s.chars[k]
	if c == nil {
		return 0, errors.New("characteristic not available")
	}
	return c.Read(buf)
}

func (s *bleSession) WriteKeys(raw []byte) error {
	c := s.chars[charKeys]
	if c == nil {
		return errors.New("characteristic not available")
	}
	_, err := c.WriteWithoutResponse(raw)
	return err
}

func (s *bleSession) Close() {
	func() {
		defer func() { _ = recover() }()
		_ = s.notifyChar.EnableNotifications(nil)
	}()
	_ = s.dev.Disconnect()
}

func findChar(s bluetooth.DeviceService, uuid bluetooth.UUID) *bluetooth.DeviceCharacteristic {
	c, err := s.DiscoverCharacteristics([]bluetooth.UUID{uuid})
	if err != nil || len(c) != 1 {
		dbg("findChar %s: err=%v n=%d\n", uuid.String(), err, len(c))
		return nil
	}
	return &c[0]
}

// probeCharacteristics is a QCYCTL_PROBE diagnostic: lists every characteristic
// of the a001 service with its properties and whether it answers a read. It
// exists because the touch-control characteristic 0x000D returns zero bytes on
// this firmware, and the only way to tell "absent" from "silent" is to enumerate.
func probeCharacteristics(svc bluetooth.DeviceService) {
	if os.Getenv("QCYCTL_PROBE") == "" {
		return
	}
	all, err := svc.DiscoverCharacteristics(nil)
	if err != nil {
		dbg("PROBE discover all: %v\n", err)
		return
	}
	dbg("PROBE %d characteristics on a001\n", len(all))
	for i := range all {
		c := all[i]
		var buf [64]byte
		n, rerr := c.Read(buf[:])
		dbg("PROBE  %s read=%d err=%v\n", c.UUID().String(), n, rerr)
	}
}

// busyMarkers are the substrings BlueZ/the stack produce when the earbuds are
// held by another central (the phone app) or the attempt is refused because of
// it. Timeouts, aborts and "not connected" are deliberately absent: they are
// what a powered-off or moved device looks like, and must reach the scan.
var busyMarkers = []string{
	"in progress",
	"inprogress",
	"already connected",
	"alreadyconnected",
	"connection refused",
	"resource busy",
	"busy",
}

func looksBusy(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	for _, s := range busyMarkers {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}
