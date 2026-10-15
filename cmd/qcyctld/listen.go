package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/bbztp/qcy-omarchy/internal/qcycore"
)

// errAlreadyRunning means another daemon owns the socket; main exits quietly.
var errAlreadyRunning = errors.New("qcyctld is already running")

// listenFdStart is the first descriptor systemd passes (SD_LISTEN_FDS_START).
const listenFdStart = 3

// activatedListener implements the stdlib part of sd_listen_fds(3). When
// LISTEN_PID names this process it returns the single socket passed in fd 3.
// (nil, nil) means the process was not socket-activated. When the variables do
// target this process but describe something unusable it returns an error and
// the caller must exit: falling back to binding the path itself would unlink
// the socket systemd owns.
func activatedListener(getenv func(string) string, pid int) (net.Listener, error) {
	lp, lf := getenv("LISTEN_PID"), getenv("LISTEN_FDS")
	if lp == "" && lf == "" {
		return nil, nil
	}
	if lp != strconv.Itoa(pid) {
		return nil, nil // inherited from a parent, not meant for us
	}
	if lf != "1" {
		return nil, fmt.Errorf("socket activation: LISTEN_FDS=%q, want exactly 1 socket", lf)
	}
	syscall.CloseOnExec(listenFdStart)
	if err := checkListeningUnixStream(listenFdStart); err != nil {
		return nil, fmt.Errorf("socket activation: fd %d: %w", listenFdStart, err)
	}
	f := os.NewFile(uintptr(listenFdStart), "qcyctld-activated")
	defer f.Close() // FileListener dups the descriptor
	l, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("socket activation: %w", err)
	}
	if ul, ok := l.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false) // the path belongs to systemd
	}
	return l, nil
}

// checkListeningUnixStream verifies fd is an AF_UNIX SOCK_STREAM socket in
// listening state.
func checkListeningUnixStream(fd int) error {
	typ, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if err != nil {
		return fmt.Errorf("not a socket: %w", err)
	}
	if typ != syscall.SOCK_STREAM {
		return fmt.Errorf("socket type %d, want SOCK_STREAM", typ)
	}
	if acc, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN); err != nil || acc != 1 {
		return errors.New("socket is not listening")
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		return err
	}
	if _, ok := sa.(*syscall.SockaddrUnix); !ok {
		return errors.New("not an AF_UNIX socket")
	}
	return nil
}

// listenFallback binds sock without systemd. It holds an exclusive lifetime
// flock on lockPath: only the holder removes a stale socket file, binds,
// chmods, and removes the socket on exit. When the lock is held, or anything
// already answers on the path (a systemd-owned socket does not take the lock),
// it returns errAlreadyRunning without touching the path. release drops the
// lock; call it after the listener was closed.
func listenFallback(sock, lockPath string) (l net.Listener, release func(), err error) {
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lf.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, nil, errAlreadyRunning
		}
		return nil, nil, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	release = func() {
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		_ = lf.Close()
	}
	if c, err := net.DialTimeout("unix", sock, 500*time.Millisecond); err == nil {
		_ = c.Close()
		release()
		return nil, nil, errAlreadyRunning
	}
	if fi, err := os.Lstat(sock); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			release()
			return nil, nil, fmt.Errorf("%s exists and is not a socket; refusing to remove it", sock)
		}
		if err := os.Remove(sock); err != nil {
			release()
			return nil, nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	l, err = net.Listen("unix", sock) // closing it unlinks the path: we hold the lock
	if err != nil {
		release()
		return nil, nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = l.Close()
		release()
		return nil, nil, fmt.Errorf("chmod socket: %w", err)
	}
	return l, release, nil
}

// listenSocket returns the socket to serve: the one systemd passed, else one
// bound under the fallback lock. release is never nil on success.
func listenSocket() (net.Listener, string, func(), error) {
	sock, err := qcycore.SocketPath(true)
	if err != nil {
		return nil, "", nil, err
	}
	l, err := activatedListener(os.Getenv, os.Getpid())
	if err != nil {
		return nil, "", nil, err
	}
	if l != nil {
		for _, k := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
			os.Unsetenv(k)
		}
		return l, sock, func() {}, nil
	}
	l, release, err := listenFallback(sock, filepath.Join(filepath.Dir(sock), "qcyctld.lock"))
	if err != nil {
		return nil, "", nil, err
	}
	return l, sock, release, nil
}
