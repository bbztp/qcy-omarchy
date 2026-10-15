package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDaemon simulates the socket, systemctl and the process table.
type fakeDaemon struct {
	t       *testing.T
	mu      sync.Mutex
	up      bool // the socket answers
	pid     int  // owner of the socket (the daemon, or systemd when sockActive)
	enabled bool // qcyctld.socket enabled
	// sockActive: systemd holds the socket; svcPID is the activated daemon
	// (0 = not activated yet).
	sockActive bool
	svcPID     int
	execs      map[string]bool
	spawned    []string
	sysCalls   []string
	killed     []int
	// spawnComesUp: the spawned/started daemon answers on the next dial.
	spawnComesUp bool
	// diesRightAway: the spawned child exits at once.
	diesRightAway bool
	mainPID       string // MainPID reported for a service that is not socket-activated
	clock         time.Time
	locks         int
	ver           string
	// alive is the process table; linger keeps a terminated pid alive for that
	// many sleeps (listener closed, process still winding down).
	alive  map[int]bool
	linger int
	// stuck: a terminated pid never exits.
	stuck bool
	// unavailable: the systemd user manager does not answer.
	unavailable bool
	// enabledUnknown: is-enabled fails without naming a state.
	enabledUnknown bool
}

func newFake(t *testing.T) (*fakeDaemon, *daemonDeps) {
	dir := t.TempDir()
	f := &fakeDaemon{t: t, execs: map[string]bool{}, spawnComesUp: true, clock: time.Unix(1000, 0), alive: map[int]bool{}}
	udir := filepath.Join(dir, "cfg", "systemd", "user")
	d := &daemonDeps{
		executable: func() (string, error) { return filepath.Join(dir, "bin", "qcyctl"), nil },
		lookPath:   func(string) (string, error) { return "", errors.New("not in PATH") },
		isExec:     func(p string) bool { return f.execs[p] },
		home:       filepath.Join(dir, "home"),
		stateDir:   filepath.Join(dir, "state"),
		lockPath:   filepath.Join(dir, "run", "start.lock"),
		unitPath:   filepath.Join(udir, serviceName),
		socketPath: filepath.Join(udir, socketName),
		logPath:    filepath.Join(dir, "state", "qcyctld.log"),
		getenv: func(k string) string {
			if k == "XDG_RUNTIME_DIR" {
				return filepath.Join(dir, "run")
			}
			return ""
		},
		now: func() time.Time { return f.clock },
	}
	d.sleep = func(dt time.Duration) {
		f.clock = f.clock.Add(dt)
		if f.linger > 0 {
			if f.linger--; f.linger == 0 {
				for p := range f.alive {
					if f.killedPID(p) {
						f.alive[p] = false
					}
				}
			}
		}
	}
	d.dial = func() error {
		if f.up {
			return nil
		}
		return errors.New("refused")
	}
	d.peerPID = func() (int, error) {
		if !f.up {
			return 0, errors.New("down")
		}
		return f.pid, nil
	}
	d.pidAlive = func(pid int) bool { return f.alive[pid] }
	d.daemonVersion = func(pid int) string {
		if f.alive[pid] || pid == f.pid {
			return f.ver
		}
		return ""
	}
	d.systemctl = func(args ...string) (string, error) {
		f.sysCalls = append(f.sysCalls, strings.Join(args, " "))
		if f.unavailable {
			return "", errors.New("Failed to connect to bus")
		}
		unit := ""
		if len(args) > 0 {
			unit = args[len(args)-1]
		}
		switch args[0] {
		case "is-system-running":
			return "running", nil
		case "is-enabled":
			if f.enabledUnknown {
				return "", errors.New("timeout")
			}
			if unit == socketName && f.enabled {
				return "enabled", nil
			}
			return "disabled", errors.New("exit 1")
		case "is-active":
			if unit == socketName && f.sockActive {
				return "active", nil
			}
			return "inactive", errors.New("exit 3")
		case "start":
			if unit == socketName {
				f.sockActive, f.up, f.pid = true, true, 1
			} else if f.spawnComesUp {
				f.up, f.pid = true, 777
			}
		case "enable":
			f.enabled, f.sockActive, f.up, f.pid = true, true, true, 1
		case "disable":
			if unit == socketName {
				f.enabled = false
				if args[1] == "--now" {
					f.sockActive, f.up = false, false
				}
			}
		case "try-restart", "restart":
			if f.svcPID != 0 {
				f.alive[f.svcPID] = false
				f.svcPID = 4242
				f.alive[4242] = true
			}
		case "stop":
			if unit == serviceName {
				if f.sockActive {
					f.alive[f.svcPID] = false
					f.svcPID = 0
				} else {
					f.up = false
					f.alive[f.pid] = false
				}
			}
		case "show":
			if f.sockActive {
				return strconv.Itoa(f.svcPID), nil
			}
			return f.mainPID, nil
		}
		return "", nil
	}
	d.spawn = func(bin, log string) (int, <-chan struct{}, error) {
		f.spawned = append(f.spawned, bin+"|"+log)
		var ch chan struct{}
		if f.diesRightAway {
			ch = make(chan struct{})
			close(ch)
			return 5, ch, nil
		}
		if f.spawnComesUp {
			f.up, f.pid = true, 555
			f.alive[555] = true
		}
		return 555, nil, nil
	}
	d.terminate = func(pid int) error {
		f.killed = append(f.killed, pid)
		f.up = false // the listener is closed at once
		if !f.stuck {
			if f.linger > 0 {
				return nil // still alive until the lingering sleeps pass
			}
			f.alive[pid] = false
		}
		return nil
	}
	d.lock = func(string, time.Duration) (func(), error) {
		f.locks++
		return func() {}, nil
	}
	return f, d
}

func (f *fakeDaemon) killedPID(p int) bool {
	for _, k := range f.killed {
		if k == p {
			return true
		}
	}
	return false
}

// running puts an ad-hoc (not systemd-held) daemon up.
func (f *fakeDaemon) running(pid int) {
	f.up, f.pid = true, pid
	f.alive[pid] = true
}

func (f *fakeDaemon) install(path string) {
	f.execs[path] = true
}

func TestResolveDaemonBinaryOrder(t *testing.T) {
	f, d := newFake(t)
	exeDir := filepath.Join(filepath.Dir(d.home), "bin")
	next := filepath.Join(exeDir, "qcyctld")
	share := filepath.Join(d.home, ".local", "share", "qcy-omarchy", "bin", "qcyctld")
	local := filepath.Join(d.home, ".local", "bin", "qcyctld")

	if _, err := d.resolveDaemonBinary(); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not found, got %v", err)
	}
	f.install(local)
	if got, _ := d.resolveDaemonBinary(); got != local {
		t.Fatalf("local: %q", got)
	}
	f.install(share)
	if got, _ := d.resolveDaemonBinary(); got != share {
		t.Fatalf("share: %q", got)
	}
	d.lookPath = func(string) (string, error) { return "/usr/bin/qcyctld", nil }
	if got, _ := d.resolveDaemonBinary(); got != share {
		t.Fatalf("share must beat PATH: %q", got)
	}
	delete(f.execs, share)
	delete(f.execs, local)
	if got, _ := d.resolveDaemonBinary(); got != "/usr/bin/qcyctld" {
		t.Fatalf("PATH: %q", got)
	}
	f.install(share)
	f.install(next)
	if got, _ := d.resolveDaemonBinary(); got != next {
		t.Fatalf("next to executable: %q", got)
	}
}

func TestStartSpawnsWhenNoService(t *testing.T) {
	f, d := newFake(t)
	f.install(filepath.Join(d.home, ".local", "bin", "qcyctld"))
	if err := startDaemon(d, 8*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(f.spawned) != 1 || !strings.HasSuffix(f.spawned[0], "|"+d.logPath) {
		t.Fatalf("spawned: %v", f.spawned)
	}
	for _, c := range f.sysCalls {
		if strings.HasPrefix(c, "start") {
			t.Fatalf("must not systemctl start: %v", f.sysCalls)
		}
	}
	// Idempotent: already up, no second spawn, no lock.
	locks := f.locks
	if err := startDaemon(d, time.Second); err != nil || len(f.spawned) != 1 || f.locks != locks {
		t.Fatalf("second start: err=%v spawned=%v", err, f.spawned)
	}
}

func TestStartPrefersEnabledSocketUnit(t *testing.T) {
	f, d := newFake(t)
	f.install(filepath.Join(d.home, ".local", "bin", "qcyctld"))
	if err := os.MkdirAll(filepath.Dir(d.socketPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.socketPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.enabled = true
	if err := startDaemon(d, 8*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(f.spawned) != 0 {
		t.Fatalf("spawned despite the service: %v", f.spawned)
	}
	want := []string{"is-enabled " + socketName, "start " + socketName}
	if strings.Join(f.sysCalls, ";") != strings.Join(want, ";") {
		t.Fatalf("systemctl calls: %v", f.sysCalls)
	}
}

func TestStartInstalledButDisabledSpawns(t *testing.T) {
	f, d := newFake(t)
	f.install(filepath.Join(d.home, ".local", "bin", "qcyctld"))
	_ = os.MkdirAll(filepath.Dir(d.socketPath), 0o755)
	_ = os.WriteFile(d.socketPath, []byte("x"), 0o644)
	if err := startDaemon(d, 8*time.Second); err != nil || len(f.spawned) != 1 {
		t.Fatalf("err=%v spawned=%v", err, f.spawned)
	}
}

func TestStartRaceLoserDoesNotSpawn(t *testing.T) {
	f, d := newFake(t)
	f.install(filepath.Join(d.home, ".local", "bin", "qcyctld"))
	// While waiting for the lock another starter brings the daemon up.
	d.lock = func(string, time.Duration) (func(), error) {
		f.up, f.pid = true, 4242
		return func() {}, nil
	}
	if err := startDaemon(d, time.Second); err != nil {
		t.Fatal(err)
	}
	if len(f.spawned) != 0 {
		t.Fatalf("race loser spawned: %v", f.spawned)
	}
}

func TestStartLockFailureAndTimeouts(t *testing.T) {
	f, d := newFake(t)
	f.install(filepath.Join(d.home, ".local", "bin", "qcyctld"))
	d.lock = func(string, time.Duration) (func(), error) { return nil, errors.New("lock busy") }
	if err := startDaemon(d, time.Second); err == nil {
		t.Fatal("want lock error")
	}
	f2, d2 := newFake(t)
	f2.install(filepath.Join(d2.home, ".local", "bin", "qcyctld"))
	f2.spawnComesUp = false
	if err := startDaemon(d2, 2*time.Second); err == nil || !strings.Contains(err.Error(), "did not accept") {
		t.Fatalf("want timeout, got %v", err)
	}
	f3, d3 := newFake(t)
	f3.install(filepath.Join(d3.home, ".local", "bin", "qcyctld"))
	f3.diesRightAway = true
	if err := startDaemon(d3, 2*time.Second); err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("want exited, got %v", err)
	}
	_, d4 := newFake(t) // no binary anywhere
	if err := startDaemon(d4, time.Second); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not found, got %v", err)
	}
}

func TestFlockSerialises(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "start.lock")
	un, err := flockPath(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flockPath(p, 250*time.Millisecond); err == nil {
		t.Fatal("second lock must time out while held")
	}
	un()
	un2, err := flockPath(p, time.Second)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	un2()
}

func TestLogRotationTruncatesOverOneMiB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state", "qcyctld.log")
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.WriteFile(p, bytes.Repeat([]byte("x"), logRotateBytes+1), 0o600)
	f, err := openLog(p)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("new")
	f.Close()
	if b, _ := os.ReadFile(p); string(b) != "new" {
		t.Fatalf("not truncated: %d bytes", len(b))
	}
	f, _ = openLog(p)
	_, _ = f.WriteString("+more")
	f.Close()
	if b, _ := os.ReadFile(p); string(b) != "new+more" {
		t.Fatalf("not appended: %q", b)
	}
}

func writeUnits(t *testing.T, d *daemonDeps, socket, service string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(d.socketPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if socket != "" {
		if err := os.WriteFile(d.socketPath, []byte(socket), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if service != "" {
		if err := os.WriteFile(d.unitPath, []byte(service), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func statusMap(t *testing.T, d *daemonDeps) map[string]interface{} {
	t.Helper()
	var out bytes.Buffer
	if err := runDaemonCmd(d, []string{"status", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestStatusJSON(t *testing.T) {
	f, d := newFake(t)
	bin := filepath.Join(d.home, ".local", "bin", "qcyctld")
	f.install(bin)
	m := statusMap(t, d)
	if m["running"] != false || m["managedBySystemd"] != false || m["serviceInstalled"] != false ||
		m["serviceEnabled"] != false || m["socketActive"] != false || m["binary"] != bin || m["log"] != d.logPath {
		t.Fatalf("stopped status: %v", m)
	}
	if _, has := m["pid"]; has {
		t.Fatalf("pid present while stopped: %v", m)
	}

	// Socket unit active, daemon activated: the pid is the service's, not the
	// pid on the other end of the connection (systemd's).
	writeUnits(t, d, "x", "x")
	f.up, f.pid, f.enabled, f.sockActive, f.svcPID = true, 1, true, true, 31337
	f.alive[31337], f.ver = true, "0.1.6"
	m = statusMap(t, d)
	if m["running"] != true || m["pid"] != float64(31337) || m["managedBySystemd"] != true ||
		m["serviceInstalled"] != true || m["serviceEnabled"] != true || m["socketActive"] != true || m["version"] != "0.1.6" {
		t.Fatalf("socket-activated status: %v", m)
	}
	// Socket held by systemd, daemon not activated yet: not running.
	f.svcPID = 0
	if m = statusMap(t, d); m["running"] != false || m["socketActive"] != true {
		t.Fatalf("idle socket: %v", m)
	}

	// An ad-hoc daemon (socket unit not active; pid differs from the service's MainPID) is not managed.
	f.sockActive, f.enabled = false, false
	f.running(4000)
	f.mainPID = "0"
	if m = statusMap(t, d); m["running"] != true || m["pid"] != float64(4000) || m["managedBySystemd"] != false {
		t.Fatalf("ad-hoc daemon: %v", m)
	}
	// A daemon started by the 0.1.5 service unit is managed.
	f.mainPID = "4000"
	if m = statusMap(t, d); m["managedBySystemd"] != true {
		t.Fatalf("0.1.5 service daemon: %v", m)
	}
}

func TestStatusReportsVersionsForDisplayOnly(t *testing.T) {
	f, d := newFake(t)
	prev := version
	version = "0.1.6"
	defer func() { version = prev }()
	if st := d.status(); st.Version != "" || st.CLIVersion != "0.1.6" {
		t.Fatalf("stopped: %+v", st)
	}
	f.running(10)
	f.ver = ""
	if st := d.status(); st.Version != "" || !st.Running {
		t.Fatalf("older daemon: %+v", st)
	}
	f.ver = "0.1.5"
	if st := d.status(); st.Version != "0.1.5" {
		t.Fatalf("version: %+v", st)
	}
	// A mismatch never restarts anything.
	if len(f.sysCalls) != 0 && strings.Contains(strings.Join(f.sysCalls, ";"), "restart") {
		t.Fatalf("status restarted: %v", f.sysCalls)
	}
}

func TestStopAdHocSendsSIGTERMToSocketOwner(t *testing.T) {
	f, d := newFake(t)
	f.running(4321)
	var out bytes.Buffer
	if err := runDaemonCmd(d, []string{"stop"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(f.killed) != 1 || f.killed[0] != 4321 {
		t.Fatalf("killed: %v", f.killed)
	}
	out.Reset()
	if err := runDaemonCmd(d, []string{"stop"}, &out); err != nil || !strings.Contains(out.String(), "not running") {
		t.Fatalf("stop when down: %v %q", err, out.String())
	}
	if len(f.killed) != 1 {
		t.Fatal("signalled a daemon that is not running")
	}
}

// Stop waits for the pid to exit, not only for the listener to close.
func TestStopWaitsForThePidToExit(t *testing.T) {
	f, d := newFake(t)
	f.running(4321)
	f.linger = 5
	start := f.clock
	if err := runDaemonCmd(d, []string{"stop"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if f.alive[4321] || f.clock.Sub(start) < 5*daemonPollEvery {
		t.Fatalf("returned while the process lingered (alive=%v waited=%v)", f.alive[4321], f.clock.Sub(start))
	}
	f.running(4322)
	f.stuck = true
	if err := runDaemonCmd(d, []string{"stop"}, &bytes.Buffer{}); err == nil {
		t.Fatal("a daemon that never exits must be an error")
	}
}

func TestStopManagedUsesSystemctlAndNeverSignals(t *testing.T) {
	f, d := newFake(t)
	writeUnits(t, d, "x", "x")
	f.enabled, f.sockActive, f.svcPID, f.up, f.pid = true, true, 99, true, 1 // pid 1 stands for systemd
	f.alive[99] = true
	if err := runDaemonCmd(d, []string{"stop"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(f.killed) != 0 || f.sysCalls[len(f.sysCalls)-1] != "stop "+serviceName {
		t.Fatalf("killed=%v calls=%v", f.killed, f.sysCalls)
	}
	if f.alive[99] || !f.up {
		t.Fatal("service stopped but the socket must keep answering")
	}
}

func installSeq(f *fakeDaemon) string {
	var seq []string
	for _, c := range f.sysCalls {
		if !strings.HasPrefix(c, "is-") && !strings.HasPrefix(c, "show") {
			seq = append(seq, c)
		}
	}
	return strings.Join(seq, ";")
}

func TestUnitTexts(t *testing.T) {
	const wantSocket = "[Unit]\nDescription=QCY earbuds BLE daemon socket (qcyctld)\n\n[Socket]\nListenStream=%t/qcyctld.sock\nSocketMode=0600\nAccept=no\nRemoveOnStop=yes\nService=qcyctld.service\n\n[Install]\nWantedBy=sockets.target\n"
	if socketUnitText != wantSocket {
		t.Fatalf("socket unit:\n%s", socketUnitText)
	}
	svc := serviceUnitText("/h/.local/share/qcy-omarchy/bin/qcyctld")
	for _, w := range []string{
		"[Unit]\nDescription=QCY earbuds BLE daemon (qcyctld)\nConditionPathExists=/h/.local/share/qcy-omarchy/bin/qcyctld\n",
		"[Service]\nType=simple\n",
		"ExecStart=\"/h/.local/share/qcy-omarchy/bin/qcyctld\"\nRestart=on-failure\nRestartSec=5\nTimeoutStopSec=20\nNoNewPrivileges=yes\n",
	} {
		if !strings.Contains(svc, w) {
			t.Fatalf("service unit lacks %q:\n%s", w, svc)
		}
	}
	if strings.Contains(svc, "[Install]") || !strings.HasSuffix(svc, "NoNewPrivileges=yes\n") {
		t.Fatalf("service unit must have no [Install] section:\n%s", svc)
	}
	// Special characters are escaped for systemd.
	q := serviceUnitText(`/h/we ird/$x%y/qcyctld`)
	if !strings.Contains(q, `ExecStart="/h/we ird/$$x%%y/qcyctld"`) || !strings.Contains(q, "ConditionPathExists=/h/we ird/$x%%y/qcyctld\n") {
		t.Fatalf("escaping:\n%s", q)
	}
}

func TestInstallServiceWritesBothUnitsAndSequence(t *testing.T) {
	f, d := newFake(t)
	bin := filepath.Join(d.home, "weird dir", "$x%y", "qcyctld")
	f.install(bin)
	d.lookPath = func(string) (string, error) { return bin, nil }
	f.running(4000) // ad-hoc daemon running (client-spawned)
	var out bytes.Buffer
	if err := runDaemonCmd(d, []string{"install-service", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(d.unitPath)
	if err != nil || string(b) != serviceUnitText(bin) {
		t.Fatalf("service unit: %v\n%s", err, b)
	}
	b, err = os.ReadFile(d.socketPath)
	if err != nil || string(b) != socketUnitText {
		t.Fatalf("socket unit: %v\n%s", err, b)
	}
	if len(f.killed) != 1 || f.killed[0] != 4000 || f.alive[4000] {
		t.Fatalf("ad-hoc daemon not stopped before systemd binds: %v", f.killed)
	}
	// The old daemon is gone before the units are written and the socket enabled.
	want := "daemon-reload;enable --now " + socketName + ";try-restart " + serviceName
	if got := installSeq(f); got != want {
		t.Fatalf("sequence: %s", got)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &m); err != nil || m["installed"] != true {
		t.Fatalf("json: %v %q", err, out.String())
	}
	// Idempotent: identical units mean no daemon-reload; the service is still
	// try-restarted (that is what picks up a new binary).
	f.sysCalls = nil
	if err := runDaemonCmd(d, []string{"install-service", "--quiet"}, &out); err != nil {
		t.Fatal(err)
	}
	if got := installSeq(f); got != "enable --now "+socketName+";try-restart "+serviceName {
		t.Fatalf("second run: %s", got)
	}
	if len(f.killed) != 1 {
		t.Fatalf("second run signalled: %v", f.killed)
	}
}

func TestInstallServiceMigratesTheOldEnabledServiceFirst(t *testing.T) {
	f, d := newFake(t)
	bin := filepath.Join(d.home, ".local", "bin", "qcyctld")
	f.install(bin)
	old := "[Unit]\nDescription=x\n\n[Service]\nExecStart=\"/x\"\n\n[Install]\nWantedBy=default.target\n"
	writeUnits(t, d, "", old)
	f.running(900)
	f.mainPID = "900" // managed by the old enabled service
	if err := runDaemonCmd(d, []string{"install-service"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := "disable " + serviceName + ";stop " + serviceName + ";daemon-reload;enable --now " + socketName + ";try-restart " + serviceName
	if got := installSeq(f); got != want {
		t.Fatalf("sequence:\n got %s\nwant %s", got, want)
	}
	if len(f.killed) != 0 {
		t.Fatalf("a systemd-managed daemon must be stopped through systemctl: %v", f.killed)
	}
	b, _ := os.ReadFile(d.unitPath)
	if strings.Contains(string(b), "[Install]") {
		t.Fatal("the unit still has an [Install] section")
	}
}

// An older daemon closes its listener, lingers, and only then removes the
// socket path: install-service must wait for the pid, and must not touch
// systemd if it never exits.
func TestInstallServiceWaitsForTheOldPidAndAbortsIfItStays(t *testing.T) {
	f, d := newFake(t)
	f.install(filepath.Join(d.home, ".local", "bin", "qcyctld"))
	f.running(4000)
	f.linger = 6
	if err := runDaemonCmd(d, []string{"install-service"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if f.alive[4000] {
		t.Fatal("old pid still alive when systemd was told to bind")
	}
	if !strings.HasPrefix(installSeq(f), "daemon-reload") {
		t.Fatalf("sequence: %s", installSeq(f))
	}

	f2, d2 := newFake(t)
	f2.install(filepath.Join(d2.home, ".local", "bin", "qcyctld"))
	f2.running(4001)
	f2.stuck = true
	err := runDaemonCmd(d2, []string{"install-service"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("want an error when the old daemon does not exit")
	}
	if installSeq(f2) != "" {
		t.Fatalf("systemd was touched: %s", installSeq(f2))
	}
	if _, serr := os.Stat(d2.socketPath); serr == nil {
		t.Fatal("units written despite the abort")
	}
}

func TestInstallServiceRestartsAnActivatedDaemonWithoutKillingIt(t *testing.T) {
	f, d := newFake(t)
	f.install(filepath.Join(d.home, ".local", "bin", "qcyctld"))
	if err := runDaemonCmd(d, []string{"install-service"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	f.svcPID = 500 // activated by a client in the meantime
	f.alive[500] = true
	f.sysCalls = nil
	if err := runDaemonCmd(d, []string{"install-service"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(f.killed) != 0 || f.alive[500] || f.svcPID != 4242 {
		t.Fatalf("killed=%v svc=%d", f.killed, f.svcPID)
	}
}

func TestInstallServiceFailsWithoutBinaryOrSystemd(t *testing.T) {
	_, d := newFake(t)
	if err := runDaemonCmd(d, []string{"install-service"}, &bytes.Buffer{}); err == nil {
		t.Fatal("want error without a binary")
	}
	f, d2 := newFake(t)
	f.install(filepath.Join(d2.home, ".local", "bin", "qcyctld"))
	f.unavailable = true
	if err := runDaemonCmd(d2, []string{"install-service"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("want 'unavailable' when systemctl fails, got %v", err)
	}
	if _, serr := os.Stat(d2.socketPath); serr == nil {
		t.Fatal("units written without a systemd user manager")
	}
	f3, d3 := newFake(t)
	f3.install(filepath.Join(d3.home, ".local", "bin", "qcyctld"))
	d3.getenv = func(string) string { return "" }
	if err := runDaemonCmd(d3, []string{"install-service"}, &bytes.Buffer{}); err == nil {
		t.Fatal("want an error without XDG_RUNTIME_DIR")
	}
}

func TestUninstallServiceSequence(t *testing.T) {
	f, d := newFake(t)
	var out bytes.Buffer
	if err := runDaemonCmd(d, []string{"uninstall-service"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(f.sysCalls) != 0 || !strings.Contains(out.String(), "not installed") {
		t.Fatalf("not installed: calls=%v out=%q", f.sysCalls, out.String())
	}
	writeUnits(t, d, "x", "x")
	if err := runDaemonCmd(d, []string{"uninstall-service"}, &out); err != nil {
		t.Fatal(err)
	}
	if got := installSeq(f); got != "disable --now "+socketName+";stop "+serviceName+";daemon-reload" {
		t.Fatalf("sequence: %s", got)
	}
	for _, p := range []string{d.socketPath, d.unitPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s not removed", p)
		}
	}
}

func TestAutostartEnabledEnv(t *testing.T) {
	get := func(v string) func(string) string { return func(string) string { return v } }
	if !autostartEnabled(get("")) || !autostartEnabled(get("0")) {
		t.Fatal("default must autostart")
	}
	if autostartEnabled(get("1")) {
		t.Fatal("QCYCTL_NO_AUTOSTART=1 must disable")
	}
}

// With the test guard on, dialDaemonStart must neither start nor touch
// anything: the offline-only paths (i18n, model status, hearing plan/result)
// never reach a daemon, and the guard answers before any autostart.
func TestDialDaemonStartRespectsGuardAndEnv(t *testing.T) {
	f, d := newFake(t)
	f.install(filepath.Join(d.home, ".local", "bin", "qcyctld"))
	old := depsFn
	depsFn = func() *daemonDeps { t.Fatal("deps requested under the hardware guard"); return d }
	defer func() { depsFn = old }()
	if _, err := dialDaemonStart(0); !errors.Is(err, errHardwareDisabled) {
		t.Fatalf("got %v", err)
	}
	if len(f.spawned) != 0 || len(f.sysCalls) != 0 {
		t.Fatal("something was started")
	}
}

func TestOfflineCommandsNeverAutostart(t *testing.T) {
	_, d := newFake(t)
	old := depsFn
	depsFn = func() *daemonDeps {
		d.spawn = func(string, string) (int, <-chan struct{}, error) {
			t.Fatal("offline command spawned the daemon")
			return 0, nil, nil
		}
		d.systemctl = func(...string) (string, error) {
			t.Fatal("offline command called systemctl")
			return "", nil
		}
		return d
	}
	defer func() { depsFn = old }()
	for _, args := range [][]string{
		{"i18n", "list", "--json"},
		{"version"},
		{"hearing", "plan"},
	} {
		func() {
			defer func() { _ = recover() }()
			switch args[0] {
			case "i18n":
				_ = cmdI18n(args[1:])
			case "version":
				_ = cmdVersion(args[1:])
			case "hearing":
				_ = cmdHearing(args[1:])
			}
		}()
	}
	// model status path must use the raw dial.
	if _, err := modelDaemonStatus(); err == nil {
		t.Fatal("model status reached a daemon under the guard")
	}
}

func TestVersionCommand(t *testing.T) {
	if err := cmdVersion([]string{"--bogus"}); err == nil {
		t.Fatal("want usage error")
	}
}

func TestVersionLineIsWhatTheBootstrapCompares(t *testing.T) {
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	prev := version
	version = "9.8.7"
	defer func() { os.Stdout = old; version = prev }()
	if err := cmdVersion(nil); err != nil {
		t.Fatal(err)
	}
	w.Close()
	buf := make([]byte, 64)
	n, _ := r.Read(buf)
	if got := string(buf[:n]); got != "qcyctl 9.8.7\n" {
		t.Fatalf("version line %q", got)
	}
}

func TestResolveDaemonBinaryPrefersStableName(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "qcy-omarchy", "bin")
	ver := filepath.Join(bin, "0.1.2")
	if err := os.MkdirAll(ver, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"qcyctl", "qcyctld"} {
		if err := os.WriteFile(filepath.Join(ver, n), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("current", n), filepath.Join(bin, n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("0.1.2", filepath.Join(bin, "current")); err != nil {
		t.Fatal(err)
	}
	d := &daemonDeps{
		executable: func() (string, error) { return filepath.Join(ver, "qcyctl"), nil },
		lookPath:   func(string) (string, error) { return "", errors.New("none") },
		isExec:     isExecutableFile,
		home:       filepath.Join(root, "home"),
	}
	got, err := d.resolveDaemonBinary()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(bin, "qcyctld"); got != want {
		t.Fatalf("got %q, want the stable %q (a unit pointing at the versioned dir breaks on upgrade)", got, want)
	}
}

func TestStableNameOnlyForPluginLayout(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// <root>/other/bin/0.1/qcyctld with a stable link, but no qcy-omarchy dir.
	ver := filepath.Join(root, "other", "bin", "0.1")
	if err := os.MkdirAll(ver, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(ver, "qcyctld")
	if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("0.1/qcyctld", filepath.Join(root, "other", "bin", "qcyctld")); err != nil {
		t.Fatal(err)
	}
	d := &daemonDeps{isExec: isExecutableFile}
	if got := d.stableName(p); got != p {
		t.Fatalf("got %q, want %q unchanged outside the qcy-omarchy/bin layout", got, p)
	}
}

func TestInstallServiceWaitsForTheSocket(t *testing.T) {
	f, d := newFake(t)
	f.install(filepath.Join(d.home, ".local", "bin", "qcyctld"))
	// systemctl enable returns before the daemon listens: it answers 3 dials later.
	calls := 0
	inner := d.dial
	d.systemctl = func(args ...string) (string, error) {
		switch args[0] {
		case "is-system-running":
			return "running", nil
		case "enable":
			f.enabled = true
		}
		return "", nil
	}
	d.dial = func() error {
		if !f.enabled {
			return errors.New("down")
		}
		if calls++; calls < 4 {
			return errors.New("not yet")
		}
		return inner()
	}
	f.up = true
	var out bytes.Buffer
	if err := runDaemonCmd(d, []string{"install-service", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	if calls < 4 {
		t.Fatalf("returned before the socket answered (dials=%d)", calls)
	}
}

func TestDisableServiceKeepsTheUnitFiles(t *testing.T) {
	f, d := newFake(t)
	f.install(filepath.Join(d.home, ".local", "bin", "qcyctld"))
	if err := runDaemonCmd(d, []string{"install-service"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	f.sysCalls = nil
	if err := runDaemonCmd(d, []string{"disable-service"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := installSeq(f); got != "disable --now "+socketName+";stop "+serviceName {
		t.Fatalf("sequence: %s", got)
	}
	for _, p := range []string{d.socketPath, d.unitPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s removed: %v", p, err)
		}
	}
	if m := statusMap(t, d); m["serviceEnabled"] != false || m["serviceInstalled"] != true {
		t.Fatalf("status: %v", m)
	}
}

func TestRefreshServiceFirstInstallAndEnabled(t *testing.T) {
	f, d := newFake(t)
	f.install(filepath.Join(d.home, ".local", "bin", "qcyctld"))
	// No socket unit file: first install.
	if err := runDaemonCmd(d, []string{"refresh-service", "--quiet"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := installSeq(f); got != "daemon-reload;enable --now "+socketName+";try-restart "+serviceName {
		t.Fatalf("first install: %s", got)
	}
	// Socket enabled and units unchanged: no reload, still try-restart.
	f.sysCalls = nil
	if err := runDaemonCmd(d, []string{"refresh-service", "--quiet"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := installSeq(f); got != "enable --now "+socketName+";try-restart "+serviceName {
		t.Fatalf("enabled: %s", got)
	}
}

func TestRefreshServiceRespectsTheOptOut(t *testing.T) {
	f, d := newFake(t)
	f.install(filepath.Join(d.home, ".local", "bin", "qcyctld"))
	writeUnits(t, d, "old socket", "old service") // units exist, socket disabled
	f.running(321)                                // client-spawned daemon from the old version
	if err := runDaemonCmd(d, []string{"refresh-service", "--quiet"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := installSeq(f); got != "daemon-reload" {
		t.Fatalf("sequence: %s (must not enable or restart)", got)
	}
	if f.enabled || f.sockActive {
		t.Fatal("the opt-out was overridden")
	}
	if b, _ := os.ReadFile(d.socketPath); string(b) != socketUnitText {
		t.Fatalf("socket unit not refreshed: %q", b)
	}
	if len(f.killed) != 1 || f.alive[321] {
		t.Fatalf("running daemon not stopped: %v", f.killed)
	}
	// Units already current: no daemon-reload, nothing to stop.
	f.sysCalls, f.killed = nil, nil
	if err := runDaemonCmd(d, []string{"refresh-service", "--quiet"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := installSeq(f); got != "" || len(f.killed) != 0 {
		t.Fatalf("second run: %q %v", got, f.killed)
	}
}

func TestRefreshServiceUnknownEnabledStateIsNotAnOptOut(t *testing.T) {
	f, d := newFake(t)
	f.install(filepath.Join(d.home, ".local", "bin", "qcyctld"))
	writeUnits(t, d, "old socket", "old service")
	f.enabledUnknown = true
	f.running(321)
	err := runDaemonCmd(d, []string{"refresh-service", "--quiet"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("want an error so the caller falls back")
	}
	if got := installSeq(f); got != "daemon-reload;try-restart "+serviceName {
		t.Fatalf("sequence: %s", got)
	}
	if len(f.killed) != 0 || !f.alive[321] {
		t.Fatalf("the daemon was stopped on an unknown state: %v", f.killed)
	}
	if f.enabled || f.sockActive {
		t.Fatal("something was enabled")
	}
}

func TestDisableServiceDisablesALeftoverOldService(t *testing.T) {
	f, d := newFake(t)
	writeUnits(t, d, "", "[Service]\nExecStart=/x\n\n[Install]\nWantedBy=default.target\n")
	if err := runDaemonCmd(d, []string{"disable-service"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := installSeq(f); got != "disable "+serviceName+";stop "+serviceName {
		t.Fatalf("sequence: %s", got)
	}
	if _, err := os.Stat(d.unitPath); err != nil {
		t.Fatal("unit file removed")
	}
}

func TestStatusWaitsBrieflyForTheVersionFile(t *testing.T) {
	f, d := newFake(t)
	f.running(10)
	calls := 0
	d.daemonVersion = func(int) string {
		if calls++; calls < 4 {
			return ""
		}
		return "0.1.6"
	}
	if st := d.status(); st.Version != "0.1.6" || calls != 4 {
		t.Fatalf("version %q after %d lookups", st.Version, calls)
	}
	// Never written (0.1.5): gives up after about a second.
	d.daemonVersion = func(int) string { return "" }
	start := f.clock
	if st := d.status(); st.Version != "" || f.clock.Sub(start) > 1100*time.Millisecond {
		t.Fatalf("version %q waited %v", st.Version, f.clock.Sub(start))
	}
}

func TestUnitPathWithControlCharactersIsRejectedBeforeAnythingIsWritten(t *testing.T) {
	for _, cmd := range []string{"install-service", "refresh-service"} {
		for _, bad := range []string{"\nExecStartPre=/bin/true", "a\rb", "a\x01b", "a\x7fb"} {
			f, d := newFake(t)
			bin := filepath.Join(d.home, "x"+bad, "qcyctld")
			f.install(bin)
			d.lookPath = func(string) (string, error) { return bin, nil }
			if err := runDaemonCmd(d, []string{cmd, "--quiet"}, &bytes.Buffer{}); err == nil {
				t.Fatalf("%s accepted %q", cmd, bad)
			}
			for _, p := range []string{d.unitPath, d.socketPath} {
				if _, err := os.Stat(p); err == nil {
					t.Fatalf("%s wrote %s for %q", cmd, p, bad)
				}
			}
			if got := installSeq(f); got != "" {
				t.Fatalf("%s touched systemd: %s", cmd, got)
			}
		}
	}
}
