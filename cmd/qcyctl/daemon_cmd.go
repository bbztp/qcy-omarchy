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

	"github.com/bbztp/qcy-omarchy/internal/qcycore"
)

// version is injected at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

const (
	serviceName      = "qcyctld.service"
	socketName       = "qcyctld.socket"
	daemonStartWait  = 8 * time.Second
	daemonPollEvery  = 100 * time.Millisecond
	daemonStopWait   = 10 * time.Second
	logRotateBytes   = 1 << 20
	noAutostartEnv   = "QCYCTL_NO_AUTOSTART"
	daemonCmdUsage   = "usage: qcyctl daemon <start|stop|status [--json]|install-service|disable-service|refresh-service|uninstall-service [--json] [--quiet]>"
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
	// pidAlive reports whether process pid still exists.
	pidAlive func(pid int) bool
	// daemonVersion is the version the daemon with this pid published ("" when
	// unknown, as for a daemon that predates it).
	daemonVersion func(pid int) string
	// socketPath is the socket unit; unitPath the service unit it activates.
	socketPath string
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
		socketPath: filepath.Join(cfg, "systemd", "user", socketName),
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
		// With the socket held by systemd the peer of a connection can be the
		// user manager itself: only ever signal something that is a qcyctld.
		comm, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
		if err != nil || strings.TrimSpace(string(comm)) != "qcyctld" {
			return fmt.Errorf("pid %d is not a qcyctld process; not signalling it", pid)
		}
		return syscall.Kill(pid, syscall.SIGTERM)
	}
	d.lock = flockPath
	d.pidAlive = func(pid int) bool {
		err := syscall.Kill(pid, 0)
		return err == nil || err == syscall.EPERM
	}
	d.daemonVersion = qcycore.ReadDaemonVersion
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

// resolveDaemonBinary finds qcyctld: next to this executable, then the managed
// data directory, ~/.local/bin and finally PATH. Under the data directory the
// versioned install (bin/<version>/) is reported by its stable name
// (bin/qcyctld) so a service unit survives upgrades.
func (d *daemonDeps) resolveDaemonBinary() (string, error) {
	var tried []string
	if exe, err := d.executable(); err == nil {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
		p := filepath.Join(filepath.Dir(exe), "qcyctld")
		tried = append(tried, p)
		if d.isExec(p) {
			return d.stableName(p), nil
		}
	}
	for _, p := range []string{
		filepath.Join(d.home, ".local", "share", "qcy-omarchy", "bin", "qcyctld"),
		filepath.Join(d.home, ".local", "bin", "qcyctld"),
	} {
		tried = append(tried, p)
		if d.isExec(p) {
			return p, nil
		}
	}
	if p, err := d.lookPath("qcyctld"); err == nil && p != "" {
		return p, nil
	}
	tried = append(tried, "$PATH")
	return "", fmt.Errorf("qcyctld not found (looked in: %s)", strings.Join(tried, ", "))
}

// stableName maps <bin>/<version>/qcyctld to <bin>/qcyctld when that stable
// name resolves to the same file, and returns p otherwise.
func (d *daemonDeps) stableName(p string) string {
	verDir := filepath.Dir(p)
	binDir := filepath.Dir(verDir)
	// Only the plugin's layout: .../qcy-omarchy/bin/<version>/<name>.
	if filepath.Base(binDir) != "bin" || filepath.Base(filepath.Dir(binDir)) != "qcy-omarchy" {
		return p
	}
	stable := filepath.Join(binDir, filepath.Base(p))
	if stable == p || !d.isExec(stable) {
		return p
	}
	r, err := filepath.EvalSymlinks(stable)
	if err != nil || r != p {
		return p
	}
	return stable
}

// serviceInstalled reports the service unit file (also the 0.1.5 one, which
// carried an [Install] section and was enabled on its own).
func (d *daemonDeps) serviceInstalled() bool {
	_, err := os.Stat(d.unitPath)
	return err == nil
}

// socketInstalled reports the socket unit file: the daemon is then started by
// systemd on the first connection.
func (d *daemonDeps) socketInstalled() bool {
	_, err := os.Stat(d.socketPath)
	return err == nil
}

// socketState reports whether the socket unit is enabled. known is false when
// systemctl failed without naming a state (no bus, timeout): the answer must
// then not be read as "disabled".
func (d *daemonDeps) socketState() (enabled, known bool) {
	if !d.socketInstalled() {
		return false, true
	}
	out, err := d.systemctl("is-enabled", socketName)
	if strings.HasPrefix(out, "enabled") {
		return true, true
	}
	if out == "" && err != nil {
		return false, false
	}
	return false, true
}

// socketEnabled is "the Settings toggle is on".
func (d *daemonDeps) socketEnabled() bool {
	e, _ := d.socketState()
	return e
}

// socketActive reports that systemd currently holds the daemon socket. Then
// the pid on the other end of a connection is the user manager's, never the
// daemon's, so the daemon is looked up through the service unit instead.
func (d *daemonDeps) socketActive() bool {
	if !d.socketInstalled() {
		return false
	}
	out, err := d.systemctl("is-active", socketName)
	return err == nil && strings.HasPrefix(out, "active")
}

// servicePID is the service's main pid (0 when it is not running).
func (d *daemonDeps) servicePID() int {
	out, err := d.systemctl("show", "-p", "MainPID", "--value", serviceName)
	n, perr := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || perr != nil || n < 0 {
		return 0
	}
	return n
}

// autostartEnabled is false when QCYCTL_NO_AUTOSTART is set to a non-empty,
// non-"0" value.
func autostartEnabled(getenv func(string) string) bool {
	v := strings.TrimSpace(getenv(noAutostartEnv))
	return v == "" || v == "0"
}

// startDaemon brings the daemon up if the socket is not answering: through the
// socket unit when it is enabled (systemd binds the socket, which is enough for
// clients to connect), otherwise by spawning qcyctld detached. Safe to call
// concurrently (flock) and idempotent.
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
	if d.socketEnabled() {
		if _, err := d.systemctl("start", socketName); err != nil {
			return fmt.Errorf("systemctl --user start %s: %w", socketName, err)
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
	ServiceInstalled bool   `json:"serviceInstalled"` // the socket unit file exists
	ServiceEnabled   bool   `json:"serviceEnabled"`   // the socket unit is enabled (the Settings toggle)
	SocketActive     bool   `json:"socketActive"`     // systemd holds the socket; the daemon starts on demand
	Binary           string `json:"binary"`
	Log              string `json:"log"`
	// Version is the running daemon's version ("" when stopped or too old to
	// publish it); CLIVersion is this qcyctl's. For display only.
	Version    string `json:"version"`
	CLIVersion string `json:"cliVersion"`
}

func (d *daemonDeps) status() daemonStatus {
	st := daemonStatus{Log: d.logPath, CLIVersion: version}
	if bin, err := d.resolveDaemonBinary(); err == nil {
		st.Binary = bin
	}
	st.ServiceInstalled = d.socketInstalled()
	st.ServiceEnabled = d.socketEnabled()
	st.SocketActive = d.socketActive()
	if st.SocketActive {
		// The socket answers whether or not the daemon was activated yet.
		if n := d.servicePID(); n > 0 {
			st.Running, st.ManagedBySystemd = true, true
			st.PID = &n
		}
	} else if d.dial() == nil {
		st.Running = true
		if pid, err := d.peerPID(); err == nil {
			p := pid
			st.PID = &p
			// A daemon started by the 0.1.5 service unit (enabled on its own).
			if d.serviceInstalled() && d.servicePID() == pid {
				st.ManagedBySystemd = true
			}
		}
	}
	if st.PID != nil && d.daemonVersion != nil {
		// A just-activated daemon writes its version file a moment after it
		// listens: give it ~1 s before reporting "unknown".
		for i := 0; i < 10; i++ {
			if st.Version = d.daemonVersion(*st.PID); st.Version != "" {
				break
			}
			d.sleep(daemonPollEvery)
		}
	}
	return st
}

// stopDaemon asks a running daemon to exit (`systemctl stop` when systemd
// manages it, otherwise SIGTERM to the pid that owns the socket) and waits for
// that pid to be gone, not merely for the listener to close: an older daemon
// closes it first and removes its socket file only when it finally exits. With
// the socket held by systemd the socket keeps answering, so only the pid counts.
// Returns whether a daemon was running.
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
	for {
		gone := true
		if st.PID != nil && d.pidAlive != nil && d.pidAlive(*st.PID) {
			gone = false
		}
		if !st.SocketActive && d.dial() == nil {
			gone = false
		}
		if gone {
			return true, nil
		}
		if !d.now().Before(deadline) {
			return true, errors.New("the daemon did not exit after the stop request")
		}
		d.sleep(daemonPollEvery)
	}
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

// socketUnitText is the socket unit: systemd binds the daemon socket and
// starts qcyctld.service on the first connection. The path equals
// qcycore.SocketPath when XDG_RUNTIME_DIR is set (%t).
const socketUnitText = `[Unit]
Description=QCY earbuds BLE daemon socket (qcyctld)

[Socket]
ListenStream=%t/qcyctld.sock
SocketMode=0600
Accept=no
RemoveOnStop=yes
Service=qcyctld.service

[Install]
WantedBy=sockets.target
`

// checkUnitPath rejects a binary path that cannot be written into a unit file
// safely: a newline or any other control character would let the path start
// new directives.
func checkUnitPath(bin string) error {
	for _, r := range bin {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("the daemon path %q contains a control character; refusing to write it into a systemd unit", bin)
		}
	}
	return nil
}

// serviceUnitText renders the service unit for the resolved qcyctld path. It
// has no [Install] section: the socket activates it.
func serviceUnitText(bin string) string {
	q := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`).Replace(bin)
	// The condition keeps a removed data directory from leaving a unit that
	// fails in a restart loop.
	cond := strings.NewReplacer(`%`, `%%`).Replace(bin)
	return "[Unit]\nDescription=" + unitDescription + "\nConditionPathExists=" + cond + "\n\n[Service]\nType=simple\n" +
		unitIdleComments +
		"ExecStart=\"" + q + "\"\nRestart=on-failure\nRestartSec=5\nTimeoutStopSec=20\nNoNewPrivileges=yes\n"
}

// writeUnit replaces the unit file atomically and reports whether the content
// changed.
func writeUnit(path, content string) (bool, error) {
	if b, err := os.ReadFile(path); err == nil && string(b) == content {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, qcycore.WriteFileAtomic(path, []byte(content), 0o644)
}

// oldUnitWithInstall reports a 0.1.5-style service unit, which carries an
// [Install] section and was enabled on its own.
func (d *daemonDeps) oldUnitWithInstall() bool {
	b, err := os.ReadFile(d.unitPath)
	return err == nil && strings.Contains(string(b), "[Install]")
}

// disableService is the Settings toggle OFF: the socket stops listening and the
// service stops, but the unit files stay so an update knows the user opted out.
func disableService(d *daemonDeps) error {
	var derr error
	if d.socketInstalled() {
		if _, err := d.systemctl("disable", "--now", socketName); err != nil {
			derr = fmt.Errorf("systemctl --user disable --now %s: %w", socketName, err)
		}
	}
	// A leftover 0.1.5 service enabled on its own (no socket unit yet).
	if d.oldUnitWithInstall() {
		if _, err := d.systemctl("disable", serviceName); err != nil && derr == nil {
			derr = fmt.Errorf("systemctl --user disable %s: %w", serviceName, err)
		}
	}
	if !d.socketInstalled() && !d.serviceInstalled() {
		return derr
	}
	if _, err := d.systemctl("stop", serviceName); err != nil && derr == nil {
		derr = fmt.Errorf("systemctl --user stop %s: %w", serviceName, err)
	}
	return derr
}

// refreshService is what the bootstrap runs after an update. No socket unit
// file: first install, so install. Socket enabled: install again (rewrites
// changed units, try-restarts the service). Socket disabled: the user opted
// out; only rewrite changed units (plus daemon-reload), never enable, and stop
// the running daemon so the next client starts the new binary.
func refreshService(d *daemonDeps) error {
	if bin, err := d.resolveDaemonBinary(); err == nil {
		if err := checkUnitPath(bin); err != nil {
			return err
		}
	}
	if !d.socketInstalled() {
		_, err := installService(d)
		return err
	}
	enabled, known := d.socketState()
	if enabled {
		_, err := installService(d)
		return err
	}
	if !known {
		// Cannot tell whether the user opted out: keep units current and restart
		// a running service, but never decide; the error makes the caller fall back.
		if bin, err := d.resolveDaemonBinary(); err == nil {
			c1, _ := writeUnit(d.unitPath, serviceUnitText(bin))
			c2, _ := writeUnit(d.socketPath, socketUnitText)
			if c1 || c2 {
				_, _ = d.systemctl("daemon-reload")
			}
		}
		_, _ = d.systemctl("try-restart", serviceName)
		return errors.New("could not tell whether " + socketName + " is enabled; units refreshed, nothing enabled or disabled")
	}
	if out, _ := d.systemctl("is-system-running"); out != "" && out != "offline" {
		if bin, err := d.resolveDaemonBinary(); err == nil {
			c1, e1 := writeUnit(d.unitPath, serviceUnitText(bin))
			c2, e2 := writeUnit(d.socketPath, socketUnitText)
			if e1 != nil || e2 != nil {
				return errors.Join(e1, e2)
			}
			if c1 || c2 {
				if _, err := d.systemctl("daemon-reload"); err != nil {
					return fmt.Errorf("systemctl --user daemon-reload: %w", err)
				}
			}
		}
	}
	_, err := stopDaemon(d, d.status())
	return err
}

// installService makes the daemon start on demand through systemd. Order
// matters: the 0.1.5 unit is disabled first; an older daemon that holds the
// socket is stopped and awaited (it removes its socket file when it exits, which
// would otherwise delete the one systemd is about to bind); then the units are
// written, systemd reloads, the socket is enabled and a running service is
// restarted so it picks up a new binary.
func installService(d *daemonDeps) (bin string, err error) {
	if bin, err = d.resolveDaemonBinary(); err != nil {
		return "", err
	}
	if err := checkUnitPath(bin); err != nil {
		return "", err
	}
	if rt := d.getenv("XDG_RUNTIME_DIR"); rt == "" || !filepath.IsAbs(rt) {
		return "", errors.New("XDG_RUNTIME_DIR is not set: no systemd user session here")
	}
	if out, _ := d.systemctl("is-system-running"); out == "" || out == "offline" {
		return "", errors.New("the systemd user manager is unavailable; the daemon starts on demand without it")
	}
	if d.oldUnitWithInstall() {
		if _, err := d.systemctl("disable", serviceName); err != nil {
			return "", fmt.Errorf("systemctl --user disable %s: %w", serviceName, err)
		}
	}
	if st := d.status(); st.Running && !st.SocketActive {
		if _, err := stopDaemon(d, st); err != nil {
			return "", fmt.Errorf("stopping the running daemon: %w", err)
		}
	}
	c1, err := writeUnit(d.unitPath, serviceUnitText(bin))
	if err != nil {
		return "", err
	}
	c2, err := writeUnit(d.socketPath, socketUnitText)
	if err != nil {
		return "", err
	}
	if c1 || c2 {
		if _, err := d.systemctl("daemon-reload"); err != nil {
			return "", fmt.Errorf("systemctl --user daemon-reload: %w (units written to %s)", err, filepath.Dir(d.unitPath))
		}
	}
	if _, err := d.systemctl("enable", "--now", socketName); err != nil {
		return "", fmt.Errorf("systemctl --user enable --now %s: %w (units written to %s)", socketName, err, filepath.Dir(d.unitPath))
	}
	if _, err := d.systemctl("try-restart", serviceName); err != nil {
		return "", fmt.Errorf("systemctl --user try-restart %s: %w", serviceName, err)
	}
	// `enable --now` returns once the socket is bound; wait so a status read
	// right after sees it accepting.
	for deadline := d.now().Add(daemonStartWait); d.dial() != nil && d.now().Before(deadline); {
		d.sleep(daemonPollEvery)
	}
	return bin, nil
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
	asJSON, quiet := false, false
	for _, a := range args[1:] {
		switch a {
		case "--json":
			asJSON = true
		case "--quiet":
			quiet = true
		default:
			return errors.New(daemonCmdUsage)
		}
	}
	emit := func(v map[string]interface{}, human string) error {
		if quiet {
			return nil
		}
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
		bin, err := installService(d)
		if err != nil {
			return err
		}
		return emit(map[string]interface{}{"installed": true, "enabled": true, "unit": d.socketPath, "service": d.unitPath, "binary": bin},
			fmt.Sprintf("%s enabled; %s starts on demand (ExecStart=%s)", socketName, serviceName, bin))
	case "disable-service":
		if err := disableService(d); err != nil {
			return err
		}
		return emit(map[string]interface{}{"enabled": false}, socketName+" disabled; unit files kept")
	case "refresh-service":
		if err := refreshService(d); err != nil {
			return err
		}
		return emit(map[string]interface{}{"refreshed": true}, "daemon service refreshed")
	case "uninstall-service":
		if !d.socketInstalled() && !d.serviceInstalled() {
			return emit(map[string]interface{}{"installed": false, "removed": false}, socketName+" is not installed")
		}
		var derr error
		note := func(err error, what string) {
			if err != nil && derr == nil {
				derr = fmt.Errorf("systemctl --user %s: %w", what, err)
			}
		}
		if d.socketInstalled() {
			_, err := d.systemctl("disable", "--now", socketName)
			note(err, "disable --now "+socketName)
		}
		if d.oldUnitWithInstall() {
			_, err := d.systemctl("disable", serviceName)
			note(err, "disable "+serviceName)
		}
		if d.serviceInstalled() {
			_, err := d.systemctl("stop", serviceName)
			note(err, "stop "+serviceName)
		}
		for _, p := range []string{d.socketPath, d.unitPath} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		_, err := d.systemctl("daemon-reload")
		note(err, "daemon-reload")
		if derr != nil {
			return derr
		}
		return emit(map[string]interface{}{"installed": false, "removed": true},
			socketName+" removed; the daemon is started on demand again")
	}
	return errors.New(daemonCmdUsage)
}
