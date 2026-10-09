package main

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	quicky "github.com/hui1601/Quicky/lib"
	"tinygo.org/x/bluetooth"
)

var (
	svcUUID      = mustUUID("0000a001-0000-1000-8000-00805f9b34fb")
	cmdUUID      = mustUUID("00001001-0000-1000-8000-00805f9b34fb")
	notifyUUID   = mustUUID("00001002-0000-1000-8000-00805f9b34fb")
	battUUID     = mustUUID("00000008-0000-1000-8000-00805f9b34fb")
	verUUID      = mustUUID("00000007-0000-1000-8000-00805f9b34fb")
	keysUUID     = mustUUID("0000000d-0000-1000-8000-00805f9b34fb")
	eqPresetUUID = mustUUID("0000000b-0000-1000-8000-00805f9b34fb")

	// N70 ANC opcode is 0x17 with 3 params [mode, scene, value].
	// "anc" is an alias for "adaptive".
	ancModes = map[string][3]byte{
		"off":          {0x02, 0x00, 0x00},
		"adaptive":     {0x01, 0x05, 0x00},
		"anc":          {0x01, 0x05, 0x00},
		"indoor":       {0x01, 0x01, 0x02},
		"commuting":    {0x01, 0x02, 0x02},
		"noisy":        {0x01, 0x03, 0x02},
		"antiwind":     {0x01, 0x04, 0x00},
		"transparency": {0x03, 0x01, 0x04},
	}

	// Toggle opcodes: param [0x01]=on, [0x02]=off.
	toggles = map[string]byte{
		"game":       0x09,
		"sleep":      0x10,
		"ldac":       0x23,
		"multipoint": 0x24,
		"wind":       0x2A,
	}

	eqFreqs = [10]uint16{31, 62, 125, 250, 500, 1000, 2000, 4000, 8000, 16000}
)

func mustUUID(s string) bluetooth.UUID {
	u, err := bluetooth.ParseUUID(s)
	if err != nil {
		panic("bad UUID " + s + ": " + err.Error())
	}
	return u
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "scan":
		err = cmdScan(args)
	case "info":
		err = cmdInfo(args)
	case "status":
		err = cmdStatus(args)
	case "noise":
		err = cmdNoise(args)
	case "anc":
		err = cmdAnc(args)
	case "game", "sleep", "ldac", "multipoint", "wind":
		err = cmdToggle(cmd, args)
	case "wear":
		err = cmdWear(args)
	case "find":
		err = cmdFind(args)
	case "prompt-vol":
		err = cmdPromptVol(args)
	case "auto-off":
		err = cmdAutoOff(args)
	case "balance":
		err = cmdBalance(args)
	case "eq":
		err = cmdEq(args)
	case "keys":
		err = cmdKeys(args)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "qcyctl: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  qcyctl scan [seconds]
  qcyctl info [seconds]
  qcyctl status [seconds]
  qcyctl noise [seconds]
  qcyctl anc <off|adaptive|indoor|commuting|noisy|antiwind|transparency> [seconds]  (anc = adaptive)
  qcyctl <game|sleep|ldac|multipoint|wind> <on|off> [seconds]
  qcyctl wear <on|off> [seconds]
  qcyctl find [seconds]
  qcyctl prompt-vol <0-255> [seconds]
  qcyctl auto-off <minutes|never> [seconds]
  qcyctl balance <left|center|right|0-100> [seconds]
  qcyctl eq get [seconds]
  qcyctl eq set <g1,g2,...,g10> [seconds]        (dB, e.g. -3,0,2.5,...)
  qcyctl eq preset <n> [seconds]
  qcyctl keys get [seconds]
  qcyctl keys set <keyId:funId> [<keyId:funId> ...] [seconds]
All setters read back by querying the opcode (FF 03 FE 01 <opcode>) and fail non-zero on mismatch.`)
}

func parseSeconds(args []string, def int) (int, error) {
	if len(args) == 0 {
		return def, nil
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid seconds %q", args[0])
	}
	return n, nil
}

func parseOnOff(s string) (byte, error) {
	switch s {
	case "on":
		return 0x01, nil
	case "off":
		return 0x02, nil
	}
	return 0, fmt.Errorf("expected on|off, got %q", s)
}

// parseByte accepts decimal or hex (0x prefix or hex letters) key/fun ids.
func parseByte(s string) (byte, error) {
	s = strings.TrimSpace(s)
	base := 10
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		base = 16
		s = s[2:]
	} else if strings.ContainsAny(s, "abcdefABCDEF") {
		base = 16
	}
	v, err := strconv.ParseUint(s, base, 8)
	if err != nil {
		return 0, err
	}
	return byte(v), nil
}

// ---------- framing ----------

func buildFrame(opcode byte, params []byte) []byte {
	p := make([]byte, 0, 4+len(params))
	p = append(p, 0xff, byte(len(params)+2), opcode, byte(len(params)))
	p = append(p, params...)
	return p
}

type frame struct {
	cmd    byte
	params []byte
}

// parseFrames parses one or more FF-framed blocks. A single buffer may contain
// several frames: FF <bodyLen> <opcode> <paramLen> <params...> [ <opcode> ... ]
func parseFrames(p []byte) ([]frame, error) {
	var out []frame
	off := 0
	for off < len(p) {
		if p[off] != 0xff {
			return out, fmt.Errorf("bad SOF 0x%02x at offset %d", p[off], off)
		}
		if off+2 > len(p) {
			return out, errors.New("truncated header")
		}
		bodyLen := int(p[off+1])
		end := off + 2 + bodyLen
		if end > len(p) {
			return out, errors.New("truncated body")
		}
		body := p[off+2 : end]
		bo := 0
		for bo < len(body) {
			if bo+2 > len(body) {
				return out, errors.New("truncated block header")
			}
			op := body[bo]
			n := int(body[bo+1])
			bo += 2
			if bo+n > len(body) {
				return out, errors.New("truncated block params")
			}
			out = append(out, frame{cmd: op, params: append([]byte(nil), body[bo:bo+n]...)})
			bo += n
		}
		off = end
	}
	if len(out) == 0 {
		return nil, errors.New("no frames")
	}
	return out, nil
}

func u16le(b []byte, off int) uint16 {
	return uint16(b[off]) | uint16(b[off+1])<<8
}

func le16(v uint16) []byte { return []byte{byte(v), byte(v >> 8)} }

// ---------- scan ----------

func cmdScan(args []string) error {
	seconds, err := parseSeconds(args, 10)
	if err != nil {
		return err
	}

	if err := bluetooth.DefaultAdapter.Enable(); err != nil {
		return fmt.Errorf("enable adapter: %w", err)
	}
	scanner := quicky.NewScanner()

	stop := make(chan struct{})
	var stopOnce sync.Once
	requestStop := func() {
		stopOnce.Do(func() {
			close(stop)
			if err := scanner.StopScan(); err != nil {
				fmt.Fprintf(os.Stderr, "stop scan: %v\n", err)
			}
		})
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		timer := time.NewTimer(time.Duration(seconds) * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-sigCh:
		}
		requestStop()
	}()

	scanErr := scanner.Scan(func(r quicky.ScanResult) {
		printScanResult(r)
	})

	select {
	case <-stop:
		return nil
	default:
		if scanErr != nil {
			return fmt.Errorf("scan: %w", scanErr)
		}
		return nil
	}
}

func printScanResult(r quicky.ScanResult) {
	fmt.Printf("device %s rssi=%d name=%q\n", r.Address, r.RSSI, r.Name)
	if ad := r.Advertisement; ad != nil {
		fmt.Printf("  vendorID=0x%04x color=%d\n", ad.VendorID, ad.ColorIndex)
		fmt.Printf("  battery left=%d%% charging=%v right=%d%% charging=%v box=%d%% charging=%v\n",
			ad.LeftBattery, ad.IsLeftCharging, ad.RightBattery, ad.IsRightCharging, ad.BoxBattery, ad.IsBoxCharging)
	}
	if p, ok := r.GetProductInfo(); ok {
		fmt.Printf("  product: %s / %s\n", p.Title, p.SubTitle)
	}
}

// ---------- connection ----------

type qcy struct {
	dev      bluetooth.Device
	cmd      bluetooth.DeviceCharacteristic // 1001 write-without-response
	notify   bluetooth.DeviceCharacteristic // 1002 read+notify
	battery  *bluetooth.DeviceCharacteristic
	version  *bluetooth.DeviceCharacteristic
	keys     *bluetooth.DeviceCharacteristic
	eqPreset *bluetooth.DeviceCharacteristic
}

func (q *qcy) Close() {
	if err := q.dev.Disconnect(); err != nil {
		fmt.Fprintf(os.Stderr, "disconnect LE: %v\n", err)
	}
}

func connectQCY(scanSeconds int) (*qcy, error) {
	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		return nil, fmt.Errorf("enable adapter: %w", err)
	}

	scanner := quicky.NewScanner()
	addrCh := make(chan bluetooth.Address, 1)
	scanErrCh := make(chan error, 1)
	go func() {
		scanErrCh <- scanner.Scan(func(r quicky.ScanResult) {
			select {
			case addrCh <- r.Address:
			default:
			}
		})
	}()

	var addr bluetooth.Address
	found := false
	timer := time.NewTimer(time.Duration(scanSeconds) * time.Second)
	defer timer.Stop()
	select {
	case addr = <-addrCh:
		found = true
	case err := <-scanErrCh:
		return nil, fmt.Errorf("scan: %w", err)
	case <-timer.C:
	}
	if !found {
		_ = scanner.StopScan()
		return nil, errors.New("no QCY device found during scan")
	}

	var dev bluetooth.Device
	var err error
	for attempt := 1; attempt <= 5; attempt++ {
		dev, err = adapter.Connect(addr, bluetooth.ConnectionParams{})
		if err == nil {
			break
		}
		fmt.Fprintf(os.Stderr, "connect attempt %d to %s failed: %v\n", attempt, addr, err)
		select {
		case addr = <-addrCh:
		default:
		}
		time.Sleep(700 * time.Millisecond)
	}
	_ = scanner.StopScan()
	if err != nil {
		return nil, fmt.Errorf("connect LE %s: %w", addr, err)
	}

	services, err := dev.DiscoverServices([]bluetooth.UUID{svcUUID})
	if err != nil {
		_ = dev.Disconnect()
		return nil, fmt.Errorf("discover service a001: %w", err)
	}
	if len(services) == 0 {
		_ = dev.Disconnect()
		return nil, errors.New("QCY service a001 not found on LE device")
	}

	q := &qcy{dev: dev}
	req, err := services[0].DiscoverCharacteristics([]bluetooth.UUID{cmdUUID, notifyUUID})
	if err != nil {
		_ = dev.Disconnect()
		return nil, fmt.Errorf("required characteristics 1001/1002: %w", err)
	}
	q.cmd = req[0]
	q.notify = req[1]

	q.battery = findChar(services[0], battUUID)
	q.version = findChar(services[0], verUUID)
	q.keys = findChar(services[0], keysUUID)
	q.eqPreset = findChar(services[0], eqPresetUUID)
	return q, nil
}

func findChar(s bluetooth.DeviceService, uuid bluetooth.UUID) *bluetooth.DeviceCharacteristic {
	c, err := s.DiscoverCharacteristics([]bluetooth.UUID{uuid})
	if err != nil || len(c) != 1 {
		return nil
	}
	return &c[0]
}

// ---------- command/response client ----------

type client struct {
	q      *qcy
	frames chan frame
}

func runClient(scanSeconds int, fn func(*client) error) error {
	q, err := connectQCY(scanSeconds)
	if err != nil {
		return err
	}
	defer q.Close()

	c := &client{q: q, frames: make(chan frame, 64)}
	if err := c.start(); err != nil {
		return err
	}
	return fn(c)
}

func (c *client) start() error {
	err := c.q.notify.EnableNotifications(func(buf []byte) {
		fmt.Printf("notify[1002] raw=%x\n", buf)
		frames, perr := parseFrames(buf)
		if perr != nil {
			fmt.Printf("  (unparsed: %v)\n", perr)
			return
		}
		for _, f := range frames {
			fmt.Printf("  frame cmd=0x%02x params=%x\n", f.cmd, f.params)
			cp := frame{cmd: f.cmd, params: append([]byte(nil), f.params...)}
			select {
			case c.frames <- cp:
			default:
			}
		}
	})
	if err != nil {
		return fmt.Errorf("enable notifications 1002: %w", err)
	}
	fmt.Println("subscribed notify[1002]")
	if c.q.battery != nil {
		if err := c.q.battery.EnableNotifications(func(buf []byte) {
			fmt.Printf("notify[0008] raw=%x\n", buf)
		}); err != nil {
			fmt.Fprintf(os.Stderr, "subscribe battery 0008: %v\n", err)
		} else {
			fmt.Println("subscribed notify[0008]")
		}
	}
	return nil
}

func (c *client) drain() {
	for {
		select {
		case <-c.frames:
		default:
			return
		}
	}
}

// send writes a frame to 1001 using write-without-response (the only supported
// write type on that characteristic), falling back to with-response on error.
func (c *client) send(opcode byte, params []byte) error {
	pkt := buildFrame(opcode, params)
	n, err := c.q.cmd.WriteWithoutResponse(pkt)
	fmt.Printf("tx %x (without-response n=%d err=%v)\n", pkt, n, err)
	if err == nil {
		return nil
	}
	n2, err2 := c.q.cmd.Write(pkt)
	fmt.Printf("tx %x (with-response n=%d err=%v)\n", pkt, n2, err2)
	if err2 != nil {
		return fmt.Errorf("write 0x%02x: without-response=%v with-response=%v", opcode, err, err2)
	}
	return nil
}

func (c *client) wait(opcode byte, timeout time.Duration) ([]byte, error) {
	deadline := time.After(timeout)
	for {
		select {
		case f := <-c.frames:
			if f.cmd == opcode {
				return f.params, nil
			}
		case <-deadline:
			return nil, fmt.Errorf("no response for opcode 0x%02x within %s", opcode, timeout)
		}
	}
}

// query sends FF 03 FE 01 <opcode> and waits for the opcode response.
func (c *client) query(opcode byte, timeout time.Duration) ([]byte, error) {
	c.drain()
	if err := c.send(0xFE, []byte{opcode}); err != nil {
		return nil, err
	}
	return c.wait(opcode, timeout)
}

// setConfirm writes a setter frame then readback-confirms. The device often
// emits the pre-change state first (as the immediate query reply) and the
// post-change state afterwards (as an unsolicited notification); so we keep
// reading frames for the opcode, re-querying periodically, until one matches.
func (c *client) setConfirm(opcode byte, params []byte, timeout time.Duration, match func([]byte) bool) ([]byte, bool, error) {
	c.drain()
	if err := c.send(opcode, params); err != nil {
		return nil, false, err
	}
	deadline := time.After(timeout)
	requery := time.After(500 * time.Millisecond)
	queries := 0
	var last []byte
	for {
		select {
		case f := <-c.frames:
			if f.cmd != opcode {
				continue
			}
			last = f.params
			if match(f.params) {
				return f.params, true, nil
			}
		case <-requery:
			if queries < 5 {
				_ = c.send(0xFE, []byte{opcode})
				queries++
				requery = time.After(500 * time.Millisecond)
			}
		case <-deadline:
			if last == nil {
				return nil, false, fmt.Errorf("no response for opcode 0x%02x within %s", opcode, timeout)
			}
			return last, false, nil
		}
	}
}

// ---------- info / status ----------

func cmdInfo(args []string) error {
	scanSeconds, err := parseSeconds(args, 30)
	if err != nil {
		return err
	}
	q, err := connectQCY(scanSeconds)
	if err != nil {
		return err
	}
	defer q.Close()

	printBattery(q)
	printVersion(q)
	return nil
}

func printBattery(q *qcy) {
	if q.battery == nil {
		fmt.Fprintln(os.Stderr, "battery characteristic 0008 not available on this device")
		return
	}
	var buf [4]byte
	n, err := q.battery.Read(buf[:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "read battery 0008: %v\n", err)
		return
	}
	left, right, box, lc, rc, cc, perr := parseBattery(buf[:n])
	if perr != nil {
		fmt.Fprintf(os.Stderr, "parse battery: %v\n", perr)
		return
	}
	fmt.Printf("battery left=%d%% charging=%v right=%d%% charging=%v box=%d%% charging=%v\n",
		left, lc, right, rc, box, cc)
}

func printVersion(q *qcy) {
	if q.version == nil {
		fmt.Fprintln(os.Stderr, "version characteristic 0007 not available on this device")
		return
	}
	var buf [8]byte
	n, err := q.version.Read(buf[:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "read version 0007: %v\n", err)
		return
	}
	l, r, perr := parseVersion(buf[:n])
	if perr != nil {
		fmt.Fprintf(os.Stderr, "parse version: %v\n", perr)
		return
	}
	fmt.Printf("version left=%s right=%s\n", l, r)
}

func cmdStatus(args []string) error {
	scanSeconds, err := parseSeconds(args, 30)
	if err != nil {
		return err
	}
	return runClient(scanSeconds, func(c *client) error {
		q := c.q
		fmt.Println("=== status ===")
		printBattery(q)
		printVersion(q)
		q16 := func(op byte) ([]byte, error) { return c.query(op, 1500*time.Millisecond) }

		printQuery("anc (0x17)", q16, 0x17, func(p []byte) string {
			if len(p) < 3 {
				return fmt.Sprintf("raw=%x", p)
			}
			return fmt.Sprintf("mode=0x%02x(%s) scene=0x%02x(%s) value=0x%02x",
				p[0], noiseModeName(p[0]), p[1], noiseSceneName(p[0], p[1]), p[2])
		})
		for _, name := range []string{"game", "sleep", "ldac", "multipoint", "wind"} {
			op := toggles[name]
			printQuery(fmt.Sprintf("%s (0x%02x)", name, op), q16, op, func(p []byte) string {
				if len(p) < 1 {
					return fmt.Sprintf("raw=%x", p)
				}
				return onOffName(p[0])
			})
		}
		printQuery("prompt-vol (0x1d)", q16, 0x1D, func(p []byte) string {
			if len(p) == 0 {
				return "raw=[]"
			}
			if len(p) >= 2 {
				return fmt.Sprintf("value=%d max=%d", p[0], p[1])
			}
			return fmt.Sprintf("value=%d", p[0])
		})
		printQuery("auto-off (0x14)", q16, 0x14, func(p []byte) string {
			if len(p) < 2 {
				return fmt.Sprintf("raw=%x", p)
			}
			return fmt.Sprintf("set=%s current=%s", minutesName(u16le(p, 0)), minutesName(u16le(p, 2)))
		})
		printQuery("wear (0x2c)", q16, 0x2C, func(p []byte) string {
			if len(p) < 1 {
				return fmt.Sprintf("raw=%x", p)
			}
			s := "disabled(0x02)"
			if p[0] == 0x01 {
				s = "enabled(0x01)"
			}
			return fmt.Sprintf("%s actions=%x", s, p[1:])
		})
		printQuery("balance (0x16)", q16, 0x16, func(p []byte) string {
			if len(p) < 1 {
				return fmt.Sprintf("raw=%x", p)
			}
			return fmt.Sprintf("value=%d (0x%02x)", p[0], p[0])
		})
		printQuery("eq (0x22)", q16, 0x22, func(p []byte) string { return fmtEq(p) })
		printKeys(q)
		return nil
	})
}

func printQuery(label string, q func(byte) ([]byte, error), opcode byte, fmtFn func([]byte) string) {
	p, err := q(opcode)
	if err != nil {
		fmt.Printf("%-18s <no response: %v>\n", label, err)
		return
	}
	fmt.Printf("%-18s %s\n", label, fmtFn(p))
}

func onOffName(v byte) string {
	switch v {
	case 0x01:
		return "on(0x01)"
	case 0x02:
		return "off(0x02)"
	case 0x00:
		return "off(0x00)"
	}
	return fmt.Sprintf("0x%02x", v)
}

// onOffConfirmed reports whether a readback matches the requested on/off.
// The device reports "on" as 0x01, but reports "off" as either 0x02 or 0x00
// depending on the opcode (e.g. 0x24 multipoint reports off as 0x00).
func onOffConfirmed(state byte, got []byte) bool {
	if len(got) < 1 {
		return false
	}
	if state == 0x01 {
		return got[0] == 0x01
	}
	return got[0] == 0x02 || got[0] == 0x00
}

func minutesName(v uint16) string {
	if v == 0xFFFF {
		return "never"
	}
	return fmt.Sprintf("%dmin", v)
}

// ---------- ANC / noise ----------

func cmdAnc(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl anc <off|adaptive|indoor|commuting|noisy|antiwind|transparency> [seconds]")
	}
	modeName := args[0]
	params, ok := ancModes[modeName]
	if !ok {
		return fmt.Errorf("anc: unknown mode %q", modeName)
	}
	scanSeconds, err := parseSeconds(args[1:], 30)
	if err != nil {
		return err
	}
	return runClient(scanSeconds, func(c *client) error {
		got, ok, err := c.setConfirm(0x17, params[:], 3*time.Second, func(p []byte) bool {
			return len(p) >= 3 && p[0] == params[0] && p[1] == params[1] && p[2] == params[2]
		})
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("anc %s readback mismatch: want %x got %x", modeName, params, got)
		}
		fmt.Printf("anc=%s confirmed: mode=0x%02x(%s) scene=0x%02x(%s) value=0x%02x\n",
			modeName, got[0], noiseModeName(got[0]), got[1], noiseSceneName(got[0], got[1]), got[2])
		return nil
	})
}

func cmdNoise(args []string) error {
	if len(args) > 0 && (args[0] == "off" || isAncMode(args[0])) {
		return cmdAnc(args)
	}
	scanSeconds, err := parseSeconds(args, 30)
	if err != nil {
		return err
	}
	return runClient(scanSeconds, func(c *client) error {
		p, err := c.query(0x17, 3*time.Second)
		if err != nil {
			return fmt.Errorf("noise: %w", err)
		}
		if len(p) < 3 {
			return fmt.Errorf("noise: short response %x", p)
		}
		fmt.Printf("noise response: mode=0x%02x(%s) scene=0x%02x(%s) value=0x%02x\n",
			p[0], noiseModeName(p[0]), p[1], noiseSceneName(p[0], p[1]), p[2])
		return nil
	})
}

func isAncMode(s string) bool {
	_, ok := ancModes[s]
	return ok
}

func noiseModeName(mode byte) string {
	switch mode {
	case 0x01:
		return "ANC"
	case 0x02:
		return "Normal"
	case 0x03:
		return "Transparency"
	default:
		return "?"
	}
}

func noiseSceneName(mode, scene byte) string {
	if mode == 0x02 {
		return "off"
	}
	if mode == 0x03 {
		return "transparency"
	}
	switch scene {
	case 0x01:
		return "indoor"
	case 0x02:
		return "commuting"
	case 0x03:
		return "noisy"
	case 0x04:
		return "antiwind"
	case 0x05:
		return "adaptive"
	default:
		return "?"
	}
}

// ---------- toggles ----------

func cmdToggle(name string, args []string) error {
	opcode := toggles[name]
	if len(args) < 1 {
		return fmt.Errorf("usage: qcyctl %s <on|off> [seconds]", name)
	}
	state, err := parseOnOff(args[0])
	if err != nil {
		return err
	}
	scanSeconds, err := parseSeconds(args[1:], 30)
	if err != nil {
		return err
	}
	return runClient(scanSeconds, func(c *client) error {
		got, ok, err := c.setConfirm(opcode, []byte{state}, 3*time.Second, func(p []byte) bool {
			return onOffConfirmed(state, p)
		})
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s readback mismatch: wrote 0x%02x got %x", name, state, got)
		}
		fmt.Printf("%s=%s confirmed (0x%02x -> %s)\n", name, args[0], opcode, onOffName(got[0]))
		return nil
	})
}

func cmdWear(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl wear <on|off> [seconds]")
	}
	state, err := parseOnOff(args[0])
	if err != nil {
		return err
	}
	scanSeconds, err := parseSeconds(args[1:], 30)
	if err != nil {
		return err
	}
	return runClient(scanSeconds, func(c *client) error {
		cur, err := c.query(0x2C, 2*time.Second)
		if err != nil {
			return fmt.Errorf("wear: read current: %w", err)
		}
		if len(cur) < 3 {
			return fmt.Errorf("wear: short current response %x", cur)
		}
		fmt.Printf("wear current: enabled=0x%02x actions=%x\n", cur[0], cur[1:])
		set := append([]byte{state}, cur[1:]...)
		want := append([]byte(nil), set...)
		got, ok, err := c.setConfirm(0x2C, set, 3*time.Second, func(p []byte) bool {
			return len(p) >= len(want) && p[0] == state && bytes.Equal(p[1:len(want)], want[1:])
		})
		if err != nil {
			return err
		}
		if !ok || len(got) < 1 || got[0] != state {
			return fmt.Errorf("wear readback mismatch: want 0x%02x got %x", state, got)
		}
		fmt.Printf("wear=%s confirmed (enabled=0x%02x actions=%x)\n", args[0], got[0], got[1:])
		return nil
	})
}

func cmdFind(args []string) error {
	scanSeconds, err := parseSeconds(args, 30)
	if err != nil {
		return err
	}
	return runClient(scanSeconds, func(c *client) error {
		if err := c.send(0x05, []byte{0x01}); err != nil {
			return err
		}
		fmt.Println("find: flash on")
		time.Sleep(1500 * time.Millisecond)
		if err := c.send(0x05, []byte{0x00}); err != nil {
			return err
		}
		fmt.Println("find: flash triggered (opcode 0x05 has no state readback)")
		return nil
	})
}

// ---------- prompt volume / auto-off / balance ----------

func cmdPromptVol(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl prompt-vol <0-255> [seconds]")
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 0 || n > 255 {
		return fmt.Errorf("prompt-vol: invalid value %q (0-255)", args[0])
	}
	scanSeconds, err := parseSeconds(args[1:], 30)
	if err != nil {
		return err
	}
	return runClient(scanSeconds, func(c *client) error {
		got, ok, err := c.setConfirm(0x1D, []byte{byte(n)}, 3*time.Second, func(p []byte) bool {
			return len(p) >= 1 && p[0] == byte(n)
		})
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("prompt-vol readback mismatch: want 0x%02x got %x", n, got)
		}
		if len(got) >= 2 {
			fmt.Printf("prompt-vol=%d confirmed (max=%d)\n", got[0], got[1])
		} else {
			fmt.Printf("prompt-vol=%d confirmed\n", got[0])
		}
		return nil
	})
}

func cmdAutoOff(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl auto-off <minutes|never> [seconds]")
	}
	var want uint16 = 0xFFFF
	if args[0] != "never" {
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 0 || n > 0xFFFE {
			return fmt.Errorf("auto-off: invalid value %q (0-65534 or never)", args[0])
		}
		want = uint16(n)
	}
	scanSeconds, err := parseSeconds(args[1:], 30)
	if err != nil {
		return err
	}
	return runClient(scanSeconds, func(c *client) error {
		got, ok, err := c.setConfirm(0x14, le16(want), 3*time.Second, func(p []byte) bool {
			return len(p) >= 2 && u16le(p, 0) == want
		})
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("auto-off readback mismatch: want %04x got %x", want, got)
		}
		cur := ""
		if len(got) >= 4 {
			cur = fmt.Sprintf(" current=%s", minutesName(u16le(got, 2)))
		}
		fmt.Printf("auto-off set=%s confirmed%s\n", minutesName(u16le(got, 0)), cur)
		return nil
	})
}

func cmdBalance(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl balance <left|center|right|0-100> [seconds]")
	}
	var val byte
	switch args[0] {
	case "left":
		val = 0x00
	case "center":
		val = 0x32
	case "right":
		val = 0x64
	default:
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 0 || n > 100 {
			return fmt.Errorf("balance: invalid value %q (left|center|right|0-100)", args[0])
		}
		val = byte(n)
	}
	scanSeconds, err := parseSeconds(args[1:], 30)
	if err != nil {
		return err
	}
	return runClient(scanSeconds, func(c *client) error {
		got, ok, err := c.setConfirm(0x16, []byte{val}, 3*time.Second, func(p []byte) bool {
			return len(p) >= 1 && p[0] == val
		})
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("balance readback mismatch: want 0x%02x got %x", val, got)
		}
		fmt.Printf("balance=%s confirmed (0x%02x)\n", args[0], got[0])
		return nil
	})
}

// ---------- EQ ----------

func cmdEq(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl eq <get|set <g1..g10>|preset <n>> [seconds]")
	}
	switch args[0] {
	case "get":
		return cmdEqGet(args[1:])
	case "set":
		return cmdEqSet(args[1:])
	case "preset":
		return cmdEqPreset(args[1:])
	}
	return fmt.Errorf("eq: unknown subcommand %q", args[0])
}

func cmdEqGet(args []string) error {
	scanSeconds, err := parseSeconds(args, 30)
	if err != nil {
		return err
	}
	return runClient(scanSeconds, func(c *client) error {
		p, err := c.query(0x22, 3*time.Second)
		if err != nil {
			return fmt.Errorf("eq get: %w", err)
		}
		fmt.Printf("eq %s\n", fmtEq(p))
		return nil
	})
}

func cmdEqSet(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl eq set <g1,g2,...,g10> [seconds]")
	}
	parts := strings.Split(args[0], ",")
	if len(parts) != 10 {
		return fmt.Errorf("eq set: need 10 gains, got %d", len(parts))
	}
	var gains [10]int
	for i, s := range parts {
		db, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return fmt.Errorf("eq set: invalid gain %q", s)
		}
		centi := int(math.Round(db * 100))
		if centi > 800 {
			centi = 800
		}
		if centi < -800 {
			centi = -800
		}
		gains[i] = centi
	}
	scanSeconds, err := parseSeconds(args[1:], 30)
	if err != nil {
		return err
	}
	return runClient(scanSeconds, func(c *client) error {
		params := buildEqParams(gains)
		fmt.Printf("eq set tx params=%x\n", params)
		match := func(p []byte) bool {
			_, bands, perr := parseEq(p)
			if perr != nil || len(bands) < 10 {
				return false
			}
			for i := range gains {
				if bands[i].gain != gains[i] {
					return false
				}
			}
			return true
		}
		got, ok, err := c.setConfirm(0x22, params, 4*time.Second, match)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("eq set readback mismatch (raw=%x)", got)
		}
		gotPreset, _, _ := parseEq(got)
		fmt.Printf("eq set confirmed: preset=%d bands=%s\n", gotPreset, fmtEq(got))
		return nil
	})
}

func cmdEqPreset(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl eq preset <n> [seconds]")
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 0 || n > 255 {
		return fmt.Errorf("eq preset: invalid value %q", args[0])
	}
	scanSeconds, err := parseSeconds(args[1:], 30)
	if err != nil {
		return err
	}
	return runClient(scanSeconds, func(c *client) error {
		if c.q.eqPreset == nil {
			fmt.Println("eq preset: not supported on this model (characteristic 000B absent)")
			return nil
		}
		if _, err := c.q.eqPreset.WriteWithoutResponse([]byte{byte(n)}); err != nil {
			return fmt.Errorf("eq preset write 000B: %w", err)
		}
		var buf [4]byte
		cnt, err := c.q.eqPreset.Read(buf[:])
		if err != nil {
			return fmt.Errorf("eq preset read 000B: %w", err)
		}
		if cnt < 1 || buf[0] != byte(n) {
			return fmt.Errorf("eq preset readback mismatch: want 0x%02x got %x", n, buf[:cnt])
		}
		fmt.Printf("eq preset=%d confirmed\n", buf[0])
		return nil
	})
}

func buildEqParams(gains [10]int) []byte {
	params := []byte{0x08, 0x00, 0x00} // presetIndex=8 (custom), masterGainLo=0, masterGainHi=0
	for i := 0; i < 10; i++ {
		params = append(params, le16(eqFreqs[i])...)
		g := int16(gains[i])
		params = append(params, byte(g), byte(g>>8))
		params = append(params, le16(100)...)
		params = append(params, 0x00)
	}
	return params
}

func parseEq(p []byte) (preset byte, bands []eqBand, err error) {
	if len(p) < 3 {
		return 0, nil, fmt.Errorf("eq: too short (%d bytes)", len(p))
	}
	preset = p[0]
	n := (len(p) - 3) / 7
	if n > 10 {
		n = 10
	}
	for i := 0; i < n; i++ {
		base := 3 + i*7
		freq := int(u16le(p, base))
		gain := int(int16(u16le(p, base+2)))
		bands = append(bands, eqBand{freq: freq, gain: gain})
	}
	return preset, bands, nil
}

type eqBand struct {
	freq int
	gain int
}

func fmtEq(p []byte) string {
	preset, bands, err := parseEq(p)
	if err != nil {
		return fmt.Sprintf("raw=%x (%v)", p, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "preset=%d raw=%x bands=", preset, p)
	for i, band := range bands {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%dHz:%.2fdB", band.freq, float64(band.gain)/100)
	}
	return b.String()
}

// ---------- key functions (touch remap) ----------

func cmdKeys(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl keys <get|set <keyId:funId>...> [seconds]")
	}
	switch args[0] {
	case "get":
		return cmdKeysGet(args[1:])
	case "set":
		return cmdKeysSet(args[1:])
	}
	return fmt.Errorf("keys: unknown subcommand %q", args[0])
}

func cmdKeysGet(args []string) error {
	scanSeconds, err := parseSeconds(args, 30)
	if err != nil {
		return err
	}
	return runClient(scanSeconds, func(c *client) error {
		printKeys(c.q)
		return nil
	})
}

func printKeys(q *qcy) {
	if q.keys == nil {
		fmt.Fprintln(os.Stderr, "keys characteristic 000D not available")
		return
	}
	var buf [64]byte
	n, err := q.keys.Read(buf[:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "read keys 000D: %v\n", err)
		return
	}
	fmt.Printf("keys raw=%x\n", buf[:n])
	var pairs []string
	for i := 0; i+1 < n; i += 2 {
		pairs = append(pairs, fmt.Sprintf("%02x:%02x", buf[i], buf[i+1]))
	}
	fmt.Printf("keys pairs=%s\n", strings.Join(pairs, " "))
	if n%2 == 1 {
		fmt.Printf("keys note: odd trailing byte 0x%02x\n", buf[n-1])
	}
}

func cmdKeysSet(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: qcyctl keys set <keyId:funId> [<keyId:funId> ...] [seconds]")
	}
	var raw []byte
	var secArgs []string
	for _, a := range args {
		if !strings.Contains(a, ":") {
			secArgs = append(secArgs, a)
			continue
		}
		parts := strings.SplitN(a, ":", 2)
		k, err := parseByte(parts[0])
		if err != nil {
			return fmt.Errorf("keys set: invalid keyId %q", parts[0])
		}
		f, err := parseByte(parts[1])
		if err != nil {
			return fmt.Errorf("keys set: invalid funId %q", parts[1])
		}
		raw = append(raw, k, f)
	}
	if len(raw) == 0 {
		return errors.New("keys set: no pairs given")
	}
	scanSeconds, err := parseSeconds(secArgs, 30)
	if err != nil {
		return err
	}
	return runClient(scanSeconds, func(c *client) error {
		if c.q.keys == nil {
			return errors.New("keys characteristic 000D not available")
		}
		fmt.Printf("keys set tx=%x\n", raw)
		n, err := c.q.keys.WriteWithoutResponse(raw)
		if err != nil {
			return fmt.Errorf("keys set write 000D: %w", err)
		}
		fmt.Printf("keys set write n=%d\n", n)
		time.Sleep(150 * time.Millisecond)
		var buf [64]byte
		cnt, err := c.q.keys.Read(buf[:])
		if err != nil {
			return fmt.Errorf("keys set readback 000D: %w", err)
		}
		if cnt < len(raw) || !bytes.Equal(buf[:len(raw)], raw) {
			return fmt.Errorf("keys set readback mismatch: want %x got %x", raw, buf[:cnt])
		}
		fmt.Printf("keys set confirmed raw=%x\n", buf[:cnt])
		return nil
	})
}

// ---------- parsers ----------

func parseBattery(b []byte) (left, right, box byte, lc, rc, cc bool, err error) {
	if len(b) < 3 {
		return 0, 0, 0, false, false, false, fmt.Errorf("battery: need 3 bytes, got %d", len(b))
	}
	left, lc = b[0]&0x7f, b[0]&0x80 != 0
	right, rc = b[1]&0x7f, b[1]&0x80 != 0
	box, cc = b[2]&0x7f, b[2]&0x80 != 0
	return left, right, box, lc, rc, cc, nil
}

func parseVersion(b []byte) (string, string, error) {
	switch {
	case len(b) >= 6:
		return fmt.Sprintf("%d.%d.%d", b[0], b[1], b[2]), fmt.Sprintf("%d.%d.%d", b[3], b[4], b[5]), nil
	case len(b) >= 3:
		return fmt.Sprintf("%d.%d.%d", b[0], b[1], b[2]), "", nil
	}
	return "", "", fmt.Errorf("version: need 3 or 6 bytes, got %d", len(b))
}
