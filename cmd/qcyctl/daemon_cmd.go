package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// version is injected at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

const (
	serviceName      = "qcyctld.service"
	daemonStartWait  = 8 * time.Second
	daemonPollEvery  = 100 * time.Millisecond
	daemonStopWait   = 5 * time.Second
	logRotateBytes   = 1 << 20
	noAutostartEnv   = "QCYCTL_NO_AUTOSTART"
	daemonCmdUsage   = "usage: qcyctl daemon <start|stop|status [--json]|install-service [--json]|uninstall-service [--json]>"
	versionCmdUsage  = "usage: qcyctl version [--json]"
	unitDescription  = "QCY earbuds BLE daemon (qcyctld)"
	unitIdleComments = `# Seconds the BLE link is kept after the last request before it is released so
# the phone app can use the earbuds (0 = never release). Default 6; override
# with ` + "`systemctl --user edit qcyctld.service`" + `:
#   [Service]
#   Environment=QCYCTL_IDLE_SECONDS=6
`
)

// daemonDeps is every side effect the lifecycle code performs, so tests can
// replace it with fakes (no daemon, no systemd, no processes).
type daemonDeps struct {
	executable func() (string, error)
	lookPath   func(string) (string, error)
	isExec     func(string) bool
	home       string
	stateDir   string
	lockPath   string
	unitPath   string
	logPath    string
	// dial reports whether the daemon socket accepts a connection.
	dial func() error
	// peerPID returns the pid that owns the listening socket (SO_PEERCRED).
	peerPID func() (int, error)
	// systemctl runs `systemctl --user <args...>` and returns stdout.
	systemctl func(args ...string) (string, error)
	// spawn starts bin detached with stdout+stderr on logPath. exited (may be
	// nil) is closed when the child terminates.
	spawn func(bin, logPath string) (pid int, exited <-chan struct{}, err error)
	// terminate sends SIGTERM to pid.
	terminate func(pid int) error
	// lock takes the start lock, waiting up to wait; it returns the unlock.
	lock  func(path string, wait time.Duration) (func(), error)
	sleep func(time.Duration)
	now   func() time.Time
	// getenv reads an environment variable.
	getenv func(string) string
}

// depsFn builds the real dependencies; tests replace it.
var depsFn = realDaemonDeps

func realDaemonDeps() *daemonDeps {
	home, _ := os.UserHomeDir()
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" || !filepath.IsAbs(state) {
		state = filepath.Join(home, ".local", "state")
	}
	state = filepath.Join(state, "qcy-omarchy")
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" || !filepath.IsAbs(cfg) {
		cfg = filepath.Join(home, ".config")
	}
	lock := filepath.Join(state, "qcy-omarchy-start.lock")
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" && filepath.IsAbs(rt) {
		lock = filepath.Join(rt, "qcy-omarchy-start.lock")
	}
	d := &daemonDeps{
		executable: os.Executable,
		lookPath:   exec.LookPath,
		isExec:     isExecutableFile,
		home:       home,
		stateDir:   state,
		lockPath:   lock,
		unitPath:   filepath.Join(cfg, "systemd", "user", serviceName),
		logPath:    filepath.Join(state, "qcyctld.log"),
		sleep:      time.Sleep,
		now:        time.Now,
		getenv:     os.Getenv,
	}
	d.dial = func() error {
		if err := refuseHardware(); err != nil {
			return err
		}
		sock, err := daemonSocketPath()
		if err != nil {
			return err
		}
		c, err := net.DialTimeout("unix", sock, 500*time.Millisecond)
		if err != nil {
			return err
		}
		return c.Close()
	}
	d.peerPID = func() (int, error) {
		if err := refuseHardware(); err != nil {
			return 0, err
		}
		sock, err := daemonSocketPath()
		if err != nil {
			return 0, err
		}
		c, err := net.DialTimeout("unix", sock, 500*time.Millisecond)
		if err != nil {
			return 0, err
		}
		defer c.Close()
		return socketPeerPID(c)
	}
	d.systemctl = func(args ...string) (string, error) {
		if err := refuseHardware(); err != nil {
			return "", err
		}
		out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).Output()
		return strings.TrimSpace(string(out)), err
	}
	d.spawn = spawnDetached
	d.terminate = func(pid int) error {
		if err := refuseHardware(); err != nil {
			return err
		}
		return syscall.Kill(pid, syscall.SIGTERM)
	}
	d.lock = flockPath
	return d
}

func isExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// socketPeerPID reads the pid of the process on the other end of a unix
// connection from the kernel (SO_PEERCRED): the process that is listening.
func socketPeerPID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, errors.New("not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, serr
	}
	if cred.Pid <= 0 || int(cred.Pid) == os.Getpid() {
		return 0, errors.New("socket owner pid unavailable")
	}
	return int(cred.Pid), nil
}

// flockPath takes an exclusive flock on path (created 0600), polling up to wait.
func flockPath(path string, wait time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EINTR {
			_ = f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, errors.New("timed out waiting for the daemon start lock")
		}
		time.Sleep(daemonPollEvery)
	}
}

// openLog opens the daemon log for append, truncating it first when it grew
// past logRotateBytes.
func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fi, err := os.Stat(path); err == nil && fi.Size() > logRotateBytes {
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	return os.OpenFile(path, flags, 0o600)
}

func spawnDetached(bin, logPath string) (int, <-chan struct{}, error) {
	if err := refuseHardware(); err != nil {
		return 0, nil, err
	}
	lf, err := openLog(logPath)
	if err != nil {
		return 0, nil, err
	}
	defer lf.Close()
	cmd := exec.Command(bin)
	cmd.Stdin = nil // /dev/null
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	return cmd.Process.Pid, done, nil
}

// resolveDaemonBinary finds qcyctld: next to this executable, then PATH, then
// the install locations.
func (d *daemonDeps) resolveDaemonBinary() (string, error) {
	var tried []string
	if exe, err := d.executable(); err == nil {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
		p := filepath.Join(filepath.Dir(exe), "qcyctld")
		tried = append(tried, p)
		if d.isExec(p) {
			return p, nil
		}
	}
	if p, err := d.lookPath("qcyctld"); err == nil && p != "" {
		return p, nil
	}
	tried = append(tried, "$PATH")
	for _, p := range []string{
		filepath.Join(d.home, ".local", "share", "qcy-omarchy", "bin", "qcyctld"),
		filepath.Join(d.home, ".local", "bin", "qcyctld"),
	} {
		tried = append(tried, p)
		if d.isExec(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("qcyctld not found (looked in: %s)", strings.Join(tried, ", "))
}

func (d *daemonDeps) serviceInstalled() bool {
	_, err := os.Stat(d.unitPath)
	return err == nil
}

func (d *daemonDeps) serviceEnabled() bool {
	if !d.serviceInstalled() {
		return false
	}
	out, err := d.systemctl("is-enabled", serviceName)
	return err == nil && strings.HasPrefix(out, "enabled")
}

// autostartEnabled is false when QCYCTL_NO_AUTOSTART is set to a non-empty,
// non-"0" value.
func autostartEnabled(getenv func(string) string) bool {
	v := strings.TrimSpace(getenv(noAutostartEnv))
	return v == "" || v == "0"
}

// startDaemon brings the daemon up if it is not answering: through the user
// service when it is enabled, otherwise by spawning qcyctld detached. Safe to
// call concurrently (flock) and idempotent.
func startDaemon(d *daemonDeps, wait time.Duration) error {
	if d.dial() == nil {
		return nil
	}
	unlock, err := d.lock(d.lockPath, wait)
	if err != nil {
		return err
	}
	defer unlock()
	// Another starter may have won while this one waited for the lock.
	if d.dial() == nil {
		return nil
	}
	var exited <-chan struct{}
	if d.serviceEnabled() {
		if _, err := d.systemctl("start", serviceName); err != nil {
			return fmt.Errorf("systemctl --user start %s: %w", serviceName, err)
		}
	} else {
		bin, err := d.resolveDaemonBinary()
		if err != nil {
			return err
		}
		if _, exited, err = d.spawn(bin, d.logPath); err != nil {
			return fmt.Errorf("start %s: %w", bin, err)
		}
	}
	deadline := d.now().Add(wait)
	for {
		if d.dial() == nil {
			return nil
		}
		if exited != nil {
			select {
			case <-exited:
				return fmt.Errorf("qcyctld exited right after start (see %s)", d.logPath)
			default:
			}
		}
		if !d.now().Before(deadline) {
			return fmt.Errorf("qcyctld did not accept connections within %s (see %s)", wait, d.logPath)
		}
		d.sleep(daemonPollEvery)
	}
}

// dialDaemonStart is dialDaemon with auto-start: when the socket is not
// answering it starts the daemon (unless disabled) and dials again. Commands
// that must never start a daemon keep using dialDaemon.
func dialDaemonStart(timeout time.Duration) (*daemonClient, error) {
	dc, err := dialDaemon(timeout)
	if err == nil || errors.Is(err, errHardwareDisabled) {
		return dc, err
	}
	d := depsFn()
	if !autostartEnabled(d.getenv) {
		return nil, err
	}
	if serr := startDaemon(d, daemonStartWait); serr != nil {
		dbg("daemon autostart: %v\n", serr)
		return nil, err
	}
	return dialDaemon(timeout)
}

// ---------- qcyctl version ----------

func cmdVersion(args []string) error {
	asJSON := false
	for _, a := range args {
		if a != "--json" {
			return errors.New(versionCmdUsage)
		}
		asJSON = true
	}
	if asJSON {
		fmt.Printf("{\"version\":%q}\n", version)
		return nil
	}
	fmt.Println("qcyctl " + version) // the bootstrap and the plugin compare this exact line
	return nil
}

// ---------- qcyctl daemon ----------

type daemonStatus struct {
	Running          bool   `json:"running"`
	PID              *int   `json:"pid,omitempty"`
	ManagedBySystemd bool   `json:"managedBySystemd"`
	ServiceInstalled bool   `json:"serviceInstalled"`
	ServiceEnabled   bool   `json:"serviceEnabled"`
	Binary           string `json:"binary"`
	Log              string `json:"log"`
}

func (d *daemonDeps) status() daemonStatus {
	st := daemonStatus{Log: d.logPath}
	if bin, err := d.resolveDaemonBinary(); err == nil {
		st.Binary = bin
	}
	st.ServiceInstalled = d.serviceInstalled()
	st.ServiceEnabled = d.serviceEnabled()
	if d.dial() == nil {
		st.Running = true
		if pid, err := d.peerPID(); err == nil {
			p := pid
			st.PID = &p
			if st.ServiceInstalled {
				out, err := d.systemctl("show", "-p", "MainPID", "--value", serviceName)
				if n, perr := strconv.Atoi(strings.TrimSpace(out)); err == nil && perr == nil && n == pid {
					st.ManagedBySystemd = true
				}
			}
		}
	}
	return st
}

// stopDaemon asks a running daemon to exit (SIGTERM to the pid that owns the
// socket, or `systemctl stop` when the service manages it) and waits for the
// socket to go quiet. Returns whether a daemon was running.
func stopDaemon(d *daemonDeps, st daemonStatus) (bool, error) {
	if !st.Running {
		return false, nil
	}
	if st.ManagedBySystemd {
		if _, err := d.systemctl("stop", serviceName); err != nil {
			return true, fmt.Errorf("systemctl --user stop %s: %w", serviceName, err)
		}
	} else {
		if st.PID == nil {
			return true, errors.New("the daemon answers but its pid could not be determined; stop it manually")
		}
		if err := d.terminate(*st.PID); err != nil {
			return true, fmt.Errorf("SIGTERM to pid %d: %w", *st.PID, err)
		}
	}
	deadline := d.now().Add(daemonStopWait)
	for d.dial() == nil {
		if !d.now().Before(deadline) {
			return true, errors.New("the daemon is still answering after the stop request")
		}
		d.sleep(daemonPollEvery)
	}
	return true, nil
}

func parseJSONFlag(args []string, usage string) (bool, error) {
	asJSON := false
	for _, a := range args {
		if a != "--json" {
			return false, errors.New(usage)
		}
		asJSON = true
	}
	return asJSON, nil
}

// unitContents renders the user unit for the resolved qcyctld path.
func unitContents(bin string) string {
	q := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`).Replace(bin)
	return "[Unit]\nDescription=" + unitDescription + "\n\n[Service]\nType=simple\n" +
		unitIdleComments +
		"ExecStart=\"" + q + "\"\nRestart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n"
}

func cmdDaemon(args []string) error {
	return runDaemonCmd(depsFn(), args, os.Stdout)
}

type fileWriter interface {
	Write([]byte) (int, error)
}

func runDaemonCmd(d *daemonDeps, args []string, out fileWriter) error {
	if len(args) < 1 {
		return errors.New(daemonCmdUsage)
	}
	say := func(format string, a ...interface{}) { fmt.Fprintf(out, format+"\n", a...) }
	asJSON, err := parseJSONFlag(args[1:], daemonCmdUsage)
	if err != nil {
		return err
	}
	emit := func(v map[string]interface{}, human string) error {
		if asJSON {
			b, _ := json.Marshal(v)
			fmt.Fprintln(out, string(b))
			return nil
		}
		say("%s", human)
		return nil
	}
	switch args[0] {
	case "start":
		was := d.dial() == nil
		if err := startDaemon(d, daemonStartWait); err != nil {
			return err
		}
		if was {
			return emit(map[string]interface{}{"started": false, "running": true}, "qcyctld is already running")
		}
		return emit(map[string]interface{}{"started": true, "running": true}, "qcyctld started")
	case "stop":
		st := d.status()
		was, err := stopDaemon(d, st)
		if err != nil {
			return err
		}
		if !was {
			return emit(map[string]interface{}{"stopped": false, "running": false}, "qcyctld is not running")
		}
		return emit(map[string]interface{}{"stopped": true, "running": false}, "qcyctld stopped")
	case "status":
		st := d.status()
		if asJSON {
			b, _ := json.Marshal(st)
			fmt.Fprintln(out, string(b))
			return nil
		}
		state := "not running"
		if st.Running {
			state = "running"
			if st.PID != nil {
				state += " (pid " + strconv.Itoa(*st.PID) + ")"
			}
			if st.ManagedBySystemd {
				state += ", managed by systemd"
			}
		}
		say("qcyctld: %s", state)
		say("service installed: %v, enabled: %v", st.ServiceInstalled, st.ServiceEnabled)
		bin := st.Binary
		if bin == "" {
			bin = "(not found)"
		}
		say("binary: %s", bin)
		say("log: %s", st.Log)
		return nil
	case "install-service":
		bin, err := d.resolveDaemonBinary()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(d.unitPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(d.unitPath, []byte(unitContents(bin)), 0o644); err != nil {
			return err
		}
		if _, err := d.systemctl("daemon-reload"); err != nil {
			return fmt.Errorf("systemctl --user daemon-reload: %w (unit written to %s)", err, d.unitPath)
		}
		// An ad-hoc daemon holds the socket; the service could not start.
		if st := d.status(); st.Running && !st.ManagedBySystemd {
			if _, err := stopDaemon(d, st); err != nil {
				return fmt.Errorf("stopping the running daemon: %w", err)
			}
		}
		if _, err := d.systemctl("enable", "--now", serviceName); err != nil {
			return fmt.Errorf("systemctl --user enable --now %s: %w (unit written to %s)", serviceName, err, d.unitPath)
		}
		return emit(map[string]interface{}{"installed": true, "enabled": true, "unit": d.unitPath, "binary": bin},
			fmt.Sprintf("%s installed and started (ExecStart=%s)", serviceName, bin))
	case "uninstall-service":
		if !d.serviceInstalled() {
			return emit(map[string]interface{}{"installed": false, "removed": false}, serviceName+" is not installed")
		}
		var derr error
		if _, err := d.systemctl("disable", "--now", serviceName); err != nil {
			derr = fmt.Errorf("systemctl --user disable --now %s: %w", serviceName, err)
		}
		if err := os.Remove(d.unitPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		if _, err := d.systemctl("daemon-reload"); err != nil && derr == nil {
			derr = fmt.Errorf("systemctl --user daemon-reload: %w", err)
		}
		if derr != nil {
			return derr
		}
		return emit(map[string]interface{}{"installed": false, "removed": true},
			serviceName+" removed; the daemon is started on demand again")
	}
	return errors.New(daemonCmdUsage)
}
