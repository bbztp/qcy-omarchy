package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDaemon simulates the socket, systemctl and the process table.
type fakeDaemon struct {
	t        *testing.T
	mu       sync.Mutex
	up       bool
	pid      int
	enabled  bool
	execs    map[string]bool
	spawned  []string
	sysCalls []string
	killed   []int
	// spawnComesUp: the spawned/started daemon answers on the next dial.
	spawnComesUp bool
	// diesRightAway: the spawned child exits at once.
	diesRightAway bool
	mainPID       string
	clock         time.Time
	locks         int
}

func newFake(t *testing.T) (*fakeDaemon, *daemonDeps) {
	dir := t.TempDir()
	f := &fakeDaemon{t: t, execs: map[string]bool{}, spawnComesUp: true, clock: time.Unix(1000, 0)}
	d := &daemonDeps{
		executable: func() (string, error) { return filepath.Join(dir, "bin", "qcyctl"), nil },
		lookPath:   func(string) (string, error) { return "", errors.New("not in PATH") },
		isExec:     func(p string) bool { return f.execs[p] },
		home:       filepath.Join(dir, "home"),
		stateDir:   filepath.Join(dir, "state"),
		lockPath:   filepath.Join(dir, "run", "start.lock"),
		unitPath:   filepath.Join(dir, "cfg", "systemd", "user", serviceName),
		logPath:    filepath.Join(dir, "state", "qcyctld.log"),
		getenv:     func(string) string { return "" },
		sleep:      func(dt time.Duration) { f.clock = f.clock.Add(dt) },
		now:        func() time.Time { return f.clock },
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
	d.systemctl = func(args ...string) (string, error) {
		f.sysCalls = append(f.sysCalls, strings.Join(args, " "))
		switch args[0] {
		case "is-enabled":
			if f.enabled {
				return "enabled", nil
			}
			return "disabled", errors.New("exit 1")
		case "start":
			if f.spawnComesUp {
				f.up = true
				f.pid = 777
			}
		case "enable":
			f.enabled = true
			f.up, f.pid, f.mainPID = true, 888, "888"
		case "disable":
			f.enabled = false
			f.up = false
		case "stop":
			f.up = false
		case "show":
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
		}
		return 555, nil, nil
	}
	d.terminate = func(pid int) error {
		f.killed = append(f.killed, pid)
		f.up = false
		return nil
	}
	d.lock = func(string, time.Duration) (func(), error) {
		f.locks++
		return func() {}, nil
	}
	return f, d
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

func TestStartPrefersEnabledService(t *testing.T) {
	f, d := newFake(t)
	f.install(filepath.Join(d.home, ".local", "bin", "qcyctld"))
	if err := os.MkdirAll(filepath.Dir(d.unitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.unitPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.enabled = true
	if err := startDaemon(d, 8*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(f.spawned) != 0 {
		t.Fatalf("spawned despite the service: %v", f.spawned)
	}
	want := []string{"is-enabled " + serviceName, "start " + serviceName}
	if strings.Join(f.sysCalls, ";") != strings.Join(want, ";") {
		t.Fatalf("systemctl calls: %v", f.sysCalls)
	}
}

func TestStartInstalledButDisabledSpawns(t *testing.T) {
	f, d := newFake(t)
	f.install(filepath.Join(d.home, ".local", "bin", "qcyctld"))
	_ = os.MkdirAll(filepath.Dir(d.unitPath), 0o755)
	_ = os.WriteFile(d.unitPath, []byte("x"), 0o644)
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

func TestStatusJSON(t *testing.T) {
	f, d := newFake(t)
	bin := filepath.Join(d.home, ".local", "bin", "qcyctld")
	f.install(bin)
	var out bytes.Buffer
	if err := runDaemonCmd(d, []string{"status", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["running"] != false || m["managedBySystemd"] != false || m["serviceInstalled"] != false ||
		m["serviceEnabled"] != false || m["binary"] != bin || m["log"] != d.logPath {
		t.Fatalf("stopped status: %v", m)
	}
	if _, has := m["pid"]; has {
		t.Fatalf("pid present while stopped: %v", m)
	}

	f.up, f.pid, f.enabled, f.mainPID = true, 31337, true, "31337"
	_ = os.MkdirAll(filepath.Dir(d.unitPath), 0o755)
	_ = os.WriteFile(d.unitPath, []byte("x"), 0o644)
	out.Reset()
	if err := runDaemonCmd(d, []string{"status", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	m = nil
	_ = json.Unmarshal(out.Bytes(), &m)
	if m["running"] != true || m["pid"] != float64(31337) || m["managedBySystemd"] != true ||
		m["serviceInstalled"] != true || m["serviceEnabled"] != true {
		t.Fatalf("running status: %v", m)
	}
	// An ad-hoc daemon (pid differs from the service's MainPID) is not managed.
	f.mainPID = "0"
	out.Reset()
	_ = runDaemonCmd(d, []string{"status", "--json"}, &out)
	m = nil
	_ = json.Unmarshal(out.Bytes(), &m)
	if m["managedBySystemd"] != false {
		t.Fatalf("ad-hoc daemon reported as managed: %v", m)
	}
}

func TestStopAdHocSendsSIGTERMToSocketOwner(t *testing.T) {
	f, d := newFake(t)
	f.up, f.pid = true, 4321
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

func TestStopManagedUsesSystemctl(t *testing.T) {
	f, d := newFake(t)
	_ = os.MkdirAll(filepath.Dir(d.unitPath), 0o755)
	_ = os.WriteFile(d.unitPath, []byte("x"), 0o644)
	f.up, f.pid, f.mainPID = true, 99, "99"
	if err := runDaemonCmd(d, []string{"stop"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(f.killed) != 0 || f.sysCalls[len(f.sysCalls)-1] != "stop "+serviceName {
		t.Fatalf("killed=%v calls=%v", f.killed, f.sysCalls)
	}
}

func TestInstallServiceWritesResolvedUnitAndSequence(t *testing.T) {
	f, d := newFake(t)
	bin := filepath.Join(d.home, "weird dir", "$x%y", "qcyctld")
	f.install(bin)
	d.lookPath = func(string) (string, error) { return bin, nil }
	f.up, f.pid = true, 4000 // ad-hoc daemon running
	var out bytes.Buffer
	if err := runDaemonCmd(d, []string{"install-service", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(d.unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ConditionPathExists=" + strings.ReplaceAll(bin, "%", "%%") + "\n"; !strings.Contains(string(b), want) {
		t.Fatalf("unit lacks %q:\n%s", want, b)
	}
	want := `ExecStart="` + strings.NewReplacer("$", "$$", "%", "%%").Replace(bin) + `"`
	if !strings.Contains(string(b), want+"\n") {
		t.Fatalf("unit lacks %s:\n%s", want, b)
	}
	for _, s := range []string{"[Unit]", "[Service]", "[Install]", "WantedBy=default.target", "Restart=on-failure"} {
		if !strings.Contains(string(b), s) {
			t.Fatalf("unit lacks %q", s)
		}
	}
	if len(f.killed) != 1 || f.killed[0] != 4000 {
		t.Fatalf("ad-hoc daemon not stopped: %v", f.killed)
	}
	var seq []string
	for _, c := range f.sysCalls {
		if !strings.HasPrefix(c, "is-enabled") && !strings.HasPrefix(c, "show") {
			seq = append(seq, c)
		}
	}
	if strings.Join(seq, ";") != "daemon-reload;enable --now "+serviceName {
		t.Fatalf("sequence: %v", f.sysCalls)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &m); err != nil || m["installed"] != true {
		t.Fatalf("json: %v %q", err, out.String())
	}
}

func TestInstallServiceFailsWithoutBinaryOrSystemd(t *testing.T) {
	_, d := newFake(t)
	if err := runDaemonCmd(d, []string{"install-service"}, &bytes.Buffer{}); err == nil {
		t.Fatal("want error without a binary")
	}
	f, d2 := newFake(t)
	f.install(filepath.Join(d2.home, ".local", "bin", "qcyctld"))
	d2.systemctl = func(...string) (string, error) { return "", errors.New("no session bus") }
	if err := runDaemonCmd(d2, []string{"install-service"}, &bytes.Buffer{}); err == nil {
		t.Fatal("want error when systemctl fails")
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
	_ = os.MkdirAll(filepath.Dir(d.unitPath), 0o755)
	_ = os.WriteFile(d.unitPath, []byte("x"), 0o644)
	if err := runDaemonCmd(d, []string{"uninstall-service"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.sysCalls, ";") != "disable --now "+serviceName+";daemon-reload" {
		t.Fatalf("sequence: %v", f.sysCalls)
	}
	if _, err := os.Stat(d.unitPath); !os.IsNotExist(err) {
		t.Fatal("unit not removed")
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
