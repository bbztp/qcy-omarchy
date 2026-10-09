package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	quicky "github.com/hui1601/Quicky/lib"
	"tinygo.org/x/bluetooth"
)

const (
	svcUUID      = "0000a001-0000-1000-8000-00805f9b34fb"
	cmdUUID      = "00001001-0000-1000-8000-00805f9b34fb"
	notifyUUID   = "00001002-0000-1000-8000-00805f9b34fb"
	battUUID     = "00000008-0000-1000-8000-00805f9b34fb"
	verUUID      = "00000007-0000-1000-8000-00805f9b34fb"
	keysUUID     = "0000000d-0000-1000-8000-00805f9b34fb"
	eqPresetUUID = "0000000b-0000-1000-8000-00805f9b34fb"
)

var (
	svcUUIDParsed      = mustUUID(svcUUID)
	cmdUUIDParsed      = mustUUID(cmdUUID)
	notifyUUIDParsed   = mustUUID(notifyUUID)
	battUUIDParsed     = mustUUID(battUUID)
	verUUIDParsed      = mustUUID(verUUID)
	keysUUIDParsed     = mustUUID(keysUUID)
	eqPresetUUIDParsed = mustUUID(eqPresetUUID)
)

type daemon struct {
	mu           sync.Mutex
	adapter      *bluetooth.Adapter
	dev          bluetooth.Device
	cmdChar      bluetooth.DeviceCharacteristic
	notifyChar   bluetooth.DeviceCharacteristic
	batteryChar  *bluetooth.DeviceCharacteristic
	versionChar  *bluetooth.DeviceCharacteristic
	keysChar     *bluetooth.DeviceCharacteristic
	eqPresetChar *bluetooth.DeviceCharacteristic
	notifyCh     chan []byte
	frames       chan frame
	connected    bool
	scanSeconds  int
}

type frame struct {
	cmd    byte
	params []byte
}

type request struct {
	Cmd  string                 `json:"cmd"`
	Args map[string]interface{} `json:"args,omitempty"`
}

type response struct {
	OK    bool        `json:"ok"`
	Data  interface{} `json:"data,omitempty"`
	Error string      `json:"error,omitempty"`
}

func mustUUID(s string) bluetooth.UUID {
	u, err := bluetooth.ParseUUID(s)
	if err != nil {
		panic("bad UUID " + s + ": " + err.Error())
	}
	return u
}

func cacheFilePath() string {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".cache")
	}
	return filepath.Join(dir, "qcyctl", "le-address")
}

func loadCachedAddr() (bluetooth.Address, bool) {
	p := cacheFilePath()
	b, err := os.ReadFile(p)
	if err != nil {
		return bluetooth.Address{}, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return bluetooth.Address{}, false
	}
	var addr bluetooth.Address
	addr.Set(fields[0])
	if len(fields) > 1 && fields[1] == "1" {
		addr.SetRandom(true)
	}
	if addr.MAC == (bluetooth.MAC{}) {
		return bluetooth.Address{}, false
	}
	return addr, true
}

func saveCachedAddr(addr bluetooth.Address) {
	p := cacheFilePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	random := "0"
	if addr.IsRandom() {
		random = "1"
	}
	_ = os.WriteFile(p, []byte(addr.String()+" "+random+"\n"), 0o644)
}

func (d *daemon) connect(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.connected {
		return nil
	}

	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		return fmt.Errorf("enable adapter: %w", err)
	}
	d.adapter = adapter

	// Fast path: cached address
	if addr, ok := loadCachedAddr(); ok {
		if dev, err := d.dialLE(adapter, addr, 2*time.Second); err == nil {
			if err := d.setupDevice(dev); err == nil {
				saveCachedAddr(addr)
				d.connected = true
				return nil
			}
			_ = dev.Disconnect()
		}
	}

	// Scan path
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
	timer := time.NewTimer(time.Duration(d.scanSeconds) * time.Second)
	defer timer.Stop()
	select {
	case addr = <-addrCh:
		found = true
	case err := <-scanErrCh:
		return fmt.Errorf("scan: %w", err)
	case <-timer.C:
	}
	if !found {
		_ = scanner.StopScan()
		return errors.New("no QCY device found during scan")
	}

	var dev bluetooth.Device
	var err error
	for attempt := 1; attempt <= 5; attempt++ {
		dev, err = adapter.Connect(addr, bluetooth.ConnectionParams{})
		if err == nil {
			break
		}
		select {
		case addr = <-addrCh:
		default:
		}
		time.Sleep(700 * time.Millisecond)
	}
	_ = scanner.StopScan()
	if err != nil {
		return fmt.Errorf("connect LE %s: %w", addr, err)
	}

	if err := d.setupDevice(dev); err != nil {
		_ = dev.Disconnect()
		return err
	}
	saveCachedAddr(addr)
	d.connected = true
	return nil
}

func (d *daemon) dialLE(adapter *bluetooth.Adapter, addr bluetooth.Address, timeout time.Duration) (bluetooth.Device, error) {
	type res struct {
		dev bluetooth.Device
		err error
	}
	ch := make(chan res, 1)
	go func() {
		dev, err := adapter.Connect(addr, bluetooth.ConnectionParams{})
		ch <- res{dev, err}
	}()
	select {
	case r := <-ch:
		return r.dev, r.err
	case <-time.After(timeout):
		return bluetooth.Device{}, errors.New("connect timed out")
	}
}

func (d *daemon) setupDevice(dev bluetooth.Device) error {
	services, err := dev.DiscoverServices([]bluetooth.UUID{svcUUIDParsed})
	if err != nil {
		return fmt.Errorf("discover service a001: %w", err)
	}
	if len(services) == 0 {
		return errors.New("QCY service a001 not found on LE device")
	}
	svc := services[0]

	req, err := svc.DiscoverCharacteristics([]bluetooth.UUID{cmdUUIDParsed, notifyUUIDParsed})
	if err != nil {
		return fmt.Errorf("discover characteristics: %w", err)
	}
	if len(req) < 2 {
		return errors.New("missing command/notify characteristics")
	}
	d.cmdChar = req[0]
	d.notifyChar = req[1]

	d.batteryChar = findChar(svc, battUUIDParsed)
	d.versionChar = findChar(svc, verUUIDParsed)
	d.keysChar = findChar(svc, keysUUIDParsed)
	d.eqPresetChar = findChar(svc, eqPresetUUIDParsed)

	d.dev = dev
	d.notifyCh = make(chan []byte, 16)
	d.frames = make(chan frame, 64)

	if err := d.notifyChar.EnableNotifications(func(buf []byte) {
		d.notifyCh <- append([]byte(nil), buf...)
	}); err != nil {
		return fmt.Errorf("enable notifications 1002: %w", err)
	}

	go d.readLoop()
	return nil
}

func findChar(s bluetooth.DeviceService, uuid bluetooth.UUID) *bluetooth.DeviceCharacteristic {
	c, err := s.DiscoverCharacteristics([]bluetooth.UUID{uuid})
	if err != nil || len(c) != 1 {
		return nil
	}
	return &c[0]
}

func (d *daemon) markDisconnected() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.connected {
		d.connected = false
		if d.notifyCh != nil {
			close(d.notifyCh)
			d.notifyCh = nil
		}
	}
}

func (d *daemon) readLoop() {
	for buf := range d.notifyCh {
		frames, _ := parseFrames(buf)
		for _, f := range frames {
			d.frames <- f
		}
	}
	d.markDisconnected()
}

func parseFrames(buf []byte) ([]frame, error) {
	var out []frame
	i := 0
	for i < len(buf) {
		if i+1 >= len(buf) {
			return out, errors.New("truncated frame")
		}
		if buf[i] != 0xFF {
			return out, fmt.Errorf("expected 0xFF at %d, got 0x%02x", i, buf[i])
		}
		bodyLen := int(buf[i+1])
		if i+2+bodyLen > len(buf) {
			return out, errors.New("frame exceeds buffer")
		}
		opcode := buf[i+2]
		paramLen := bodyLen - 2
		var params []byte
		if paramLen > 0 {
			params = buf[i+3 : i+3+paramLen]
		}
		out = append(out, frame{cmd: opcode, params: params})
		i += 2 + bodyLen
	}
	return out, nil
}

func buildFrame(opcode byte, params []byte) []byte {
	bodyLen := len(params) + 2
	pkt := make([]byte, 3+bodyLen)
	pkt[0] = 0xFF
	pkt[1] = byte(bodyLen)
	pkt[2] = 0xFE
	pkt[3] = opcode
	copy(pkt[4:], params)
	return pkt
}

func (d *daemon) send(opcode byte, params []byte) error {
	pkt := buildFrame(opcode, params)
	_, err := d.cmdChar.WriteWithoutResponse(pkt)
	if err == nil {
		return nil
	}
	_, err2 := d.cmdChar.Write(pkt)
	if err2 != nil {
		d.markDisconnected()
		return fmt.Errorf("write 0x%02x: %v", opcode, err2)
	}
	return nil
}

func (d *daemon) wait(opcode byte, timeout time.Duration) ([]byte, error) {
	deadline := time.After(timeout)
	for {
		select {
		case f := <-d.frames:
			if f.cmd == opcode {
				return f.params, nil
			}
		case <-deadline:
			return nil, fmt.Errorf("no response for opcode 0x%02x within %s", opcode, timeout)
		}
	}
}

func (d *daemon) query(opcode byte, timeout time.Duration) ([]byte, error) {
	d.drain()
	if err := d.send(0xFE, []byte{opcode}); err != nil {
		return nil, err
	}
	return d.wait(opcode, timeout)
}

func (d *daemon) drain() {
	for {
		select {
		case <-d.frames:
		default:
			return
		}
	}
}

func (d *daemon) handleRequest(req request) (interface{}, error) {
	d.mu.Lock()
	if !d.connected {
		d.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := d.connect(ctx); err != nil {
			return nil, err
		}
	} else {
		d.mu.Unlock()
	}

	switch req.Cmd {
	case "status":
		return d.gatherStatus()
	case "battery":
		return d.readBattery()
	case "set":
		action, _ := req.Args["action"].(string)
		paramsRaw, _ := req.Args["params"].([]interface{})
		var params []byte
		for _, p := range paramsRaw {
			if f, ok := p.(float64); ok {
				params = append(params, byte(f))
			}
		}
		return nil, d.setAction(action, params)
	case "find":
		return nil, d.setAction("find", []byte{0x01})
	case "disconnect":
		d.mu.Lock()
		if d.connected {
			_ = d.dev.Disconnect()
			d.connected = false
		}
		d.mu.Unlock()
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown command: %s", req.Cmd)
	}
}

func (d *daemon) gatherStatus() (map[string]interface{}, error) {
	d.mu.Lock()
	if !d.connected {
		d.mu.Unlock()
		return nil, errors.New("not connected")
	}
	d.mu.Unlock()

	rep := map[string]interface{}{}
	if left, right, box, lc, rc, cc, err := d.readBatteryRaw(); err == nil {
		rep["battery"] = map[string]interface{}{
			"left": left, "right": right, "case": box,
			"leftCharging": lc, "rightCharging": rc, "caseCharging": cc,
			"casePresent": box != 0x00 && box != 0x7f,
		}
	}

	// ANC
	if p, err := d.query(0x17, 1500*time.Millisecond); err == nil && len(p) >= 3 {
		rep["anc"] = map[string]interface{}{"modeRaw": int(p[0]), "sceneRaw": int(p[1]), "value": int(p[2])}
	}

	// Toggles
	toggles := map[string]byte{"game": 0x09, "sleep": 0x10, "ldac": 0x23, "multipoint": 0x24, "wind": 0x2A}
	togRep := map[string]bool{}
	for name, op := range toggles {
		if p, err := d.query(op, 1500*time.Millisecond); err == nil && len(p) >= 1 {
			togRep[name] = p[0] == 0x01
		}
	}
	if len(togRep) > 0 {
		rep["toggles"] = togRep
	}

	// Other queries (prompt-vol, auto-off, wear, balance, EQ)
	other := map[string]byte{
		"promptVol": 0x1D, "autoOff": 0x14, "wear": 0x2C, "balance": 0x16, "eq": 0x22,
	}
	for name, op := range other {
		if p, err := d.query(op, 1500*time.Millisecond); err == nil {
			rep[name] = p
		}
	}
	return rep, nil
}

func (d *daemon) readBatteryRaw() (left, right, box byte, lc, rc, cc bool, err error) {
	if d.batteryChar == nil {
		return 0, 0, 0, false, false, false, errors.New("battery characteristic not available")
	}
	// Battery notifies on 0008; we read the characteristic directly for a snapshot
	var buf [64]byte
	n, err := d.batteryChar.Read(buf[:])
	if err != nil {
		return 0, 0, 0, false, false, false, err
	}
	if n < 3 {
		return 0, 0, 0, false, false, false, errors.New("battery data too short")
	}
	data := buf[:n]
	return data[0], data[1], data[2], data[0]&0x80 != 0, data[1]&0x80 != 0, data[2]&0x80 != 0, nil
}

func (d *daemon) readBattery() (map[string]interface{}, error) {
	left, right, box, lc, rc, cc, err := d.readBatteryRaw()
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"left": left, "right": right, "case": box,
		"leftCharging": lc, "rightCharging": rc, "caseCharging": cc,
		"casePresent": box != 0x00 && box != 0x7f,
	}, nil
}

func (d *daemon) setAction(action string, params []byte) error {
	opcodeMap := map[string]byte{
		"anc": 0x17, "game": 0x09, "sleep": 0x10, "ldac": 0x23, "multipoint": 0x24,
		"wind": 0x2A, "promptVol": 0x1D, "autoOff": 0x14, "wear": 0x2C, "balance": 0x16,
		"eq": 0x22, "find": 0x05,
	}
	opcode, ok := opcodeMap[action]
	if !ok {
		return fmt.Errorf("unknown action: %s", action)
	}

	d.mu.Lock()
	if !d.connected {
		d.mu.Unlock()
		return errors.New("not connected")
	}
	d.mu.Unlock()

	d.drain()
	if err := d.send(opcode, params); err != nil {
		return err
	}

	// For setters that need confirmation (most do), wait for the opcode response
	// Use a short timeout; if no confirmation, assume it went through (optimistic)
	_, _ = d.wait(opcode, 3*time.Second)
	return nil
}

func socketPath() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = "/tmp"
	}
	return filepath.Join(dir, "qcyctld.sock")
}

func handleConn(conn net.Conn, d *daemon) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	enc := json.NewEncoder(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = enc.Encode(response{OK: false, Error: "invalid JSON: " + err.Error()})
			continue
		}
		data, err := d.handleRequest(req)
		if err != nil {
			_ = enc.Encode(response{OK: false, Error: err.Error()})
		} else {
			_ = enc.Encode(response{OK: true, Data: data})
		}
	}
}

func main() {
	scanSeconds := 15
	if len(os.Args) > 1 {
		if v, err := strconv.Atoi(os.Args[1]); err == nil {
			scanSeconds = v
		}
	}

	sock := socketPath()
	_ = os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(sock)
	defer l.Close()

	d := &daemon{scanSeconds: scanSeconds}

	// NO auto-connect on startup. Lazy connect on first request.
	// NO periodic reconnect ticker — plugin drives via refresh() on connect.

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		_ = l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go handleConn(conn, d)
	}
}