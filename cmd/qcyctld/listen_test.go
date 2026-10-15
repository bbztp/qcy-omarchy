package main

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The helper process re-executes the test binary as a socket-activated daemon:
// the parent passes a listening socket as fd 3 (ExtraFiles) and the helper sets
// LISTEN_PID to its own pid (the parent cannot know it before the exec).
const helperEnv = "QCYCTLD_LISTEN_HELPER"

func TestMain_listenHelper(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		t.Skip("helper process only")
	}
	getenv := func(k string) string {
		switch k {
		case "LISTEN_PID":
			if mode == "otherpid" {
				return "1"
			}
			return strconv.Itoa(os.Getpid())
		case "LISTEN_FDS":
			if mode == "twofds" {
				return "2"
			}
			return "1"
		}
		return ""
	}
	l, err := activatedListener(getenv, os.Getpid())
	switch {
	case err != nil:
		os.Stdout.WriteString("ERR " + err.Error() + "\n")
		os.Exit(3)
	case l == nil:
		os.Stdout.WriteString("NOTMINE\n")
		os.Exit(0)
	}
	os.Stdout.WriteString("READY\n")
	c, err := l.Accept()
	if err != nil {
		os.Exit(4)
	}
	c.Write([]byte("hello\n"))
	c.Close()
	l.Close() // must not unlink the path systemd owns
	os.Exit(0)
}

func runHelper(t *testing.T, mode string, extra *os.File) (*exec.Cmd, *bufio.Reader) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMain_listenHelper$")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode)
	if extra != nil {
		cmd.ExtraFiles = []*os.File{extra}
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, bufio.NewReader(out)
}

func TestActivationServesFd3AndNeverUnlinksThePath(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "act.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f, err := ln.(*net.UnixListener).File()
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	defer f.Close()

	cmd, out := runHelper(t, "ok", f)
	line, _ := out.ReadString('\n')
	if line != "READY\n" {
		t.Fatalf("helper said %q", line)
	}
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := bufio.NewReader(c).ReadString('\n')
	c.Close()
	if got != "hello\n" {
		t.Fatalf("served %q", got)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper: %v", err)
	}
	if _, err := os.Lstat(sock); err != nil {
		t.Fatalf("the activated daemon removed systemd's socket path: %v", err)
	}
}

func TestActivationRejectsWhatItCannotUse(t *testing.T) {
	// LISTEN_FDS other than 1 for this pid: error, never a fallback.
	sock := filepath.Join(t.TempDir(), "a.sock")
	ln, _ := net.Listen("unix", sock)
	f, _ := ln.(*net.UnixListener).File()
	ln.Close()
	defer f.Close()
	cmd, out := runHelper(t, "twofds", f)
	if line, _ := out.ReadString('\n'); !strings.HasPrefix(line, "ERR ") {
		t.Fatalf("two fds: %q", line)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("helper exited 0 on an invalid activation")
	}

	// fd 3 is a regular file, not a socket.
	rf, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	cmd, out = runHelper(t, "ok", rf)
	if line, _ := out.ReadString('\n'); !strings.HasPrefix(line, "ERR ") {
		t.Fatalf("not a socket: %q", line)
	}
	cmd.Wait()

	// A connected (not listening) stream socket.
	a, b, err := socketPair()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer b.Close()
	cmd, out = runHelper(t, "ok", a)
	if line, _ := out.ReadString('\n'); !strings.HasPrefix(line, "ERR ") {
		t.Fatalf("not listening: %q", line)
	}
	cmd.Wait()
}

func socketPair() (*os.File, *os.File, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[0]), "a"), os.NewFile(uintptr(fds[1]), "b"), nil
}

func TestActivationIgnoresVariablesForAnotherProcess(t *testing.T) {
	cmd, out := runHelper(t, "otherpid", nil)
	if line, _ := out.ReadString('\n'); line != "NOTMINE\n" {
		t.Fatalf("got %q", line)
	}
	cmd.Wait()
	l, err := activatedListener(func(string) string { return "" }, os.Getpid())
	if l != nil || err != nil {
		t.Fatalf("no variables: %v %v", l, err)
	}
}

func TestFallbackLockSecondDaemonLeavesTheFirstSocketAlone(t *testing.T) {
	dir := t.TempDir()
	sock, lock := filepath.Join(dir, "d.sock"), filepath.Join(dir, "d.lock")
	l1, rel1, err := listenFallback(sock, lock)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Lstat(sock)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", fi.Mode().Perm())
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // keep serving
		defer wg.Done()
		for {
			c, err := l1.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if _, _, err := listenFallback(sock, lock); err != errAlreadyRunning {
		t.Fatalf("second daemon: %v, want errAlreadyRunning", err)
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("first daemon's socket was disturbed: %v", err)
	}
	c.Close()
	l1.Close()
	wg.Wait()
	rel1()
	// Lock released and path removed on close: a successor starts cleanly.
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
	l2, rel2, err := listenFallback(sock, lock)
	if err != nil {
		t.Fatal(err)
	}
	l2.Close()
	rel2()
}

func TestFallbackDoesNotTakeOverAnAnsweringSocketWithoutTheLock(t *testing.T) {
	// Stands for a systemd-owned socket: nobody holds the fallback lock.
	dir := t.TempDir()
	sock := filepath.Join(dir, "d.sock")
	sl, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	sl.(*net.UnixListener).SetUnlinkOnClose(false)
	defer sl.Close()
	if _, _, err := listenFallback(sock, filepath.Join(dir, "d.lock")); err != errAlreadyRunning {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Lstat(sock); err != nil {
		t.Fatal("socket removed")
	}
}

func TestFallbackRemovesAStaleSocketAndRefusesAFile(t *testing.T) {
	dir := t.TempDir()
	sock, lock := filepath.Join(dir, "d.sock"), filepath.Join(dir, "d.lock")
	old, _ := net.Listen("unix", sock)
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	old.Close() // stale: file stays, nobody answers
	l, rel, err := listenFallback(sock, lock)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	l.Close()
	rel()
	if err := os.WriteFile(sock, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := listenFallback(sock, lock); err == nil || err == errAlreadyRunning {
		t.Fatalf("regular file: %v", err)
	}
	if b, _ := os.ReadFile(sock); string(b) != "x" {
		t.Fatal("a regular file was removed")
	}
}

func TestBeginStopWaitsForInFlightAndRefusesNewWork(t *testing.T) {
	d := newDaemon(&fakeDialer{}, defaultLifecycle())
	if !d.tryBegin() {
		t.Fatal("tryBegin refused before the stop")
	}
	done := make(chan bool, 1)
	go func() { done <- d.beginStop(5 * time.Second) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		d.mu.Lock()
		s := d.stopping
		d.mu.Unlock()
		if s {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stop never began")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if d.tryBegin() {
		t.Fatal("new request accepted while stopping")
	}
	if _, err := d.handleRequest(request{Cmd: "status"}); err != errStopping || errorCode(err) != stoppingCode {
		t.Fatalf("handleRequest: %v", err)
	}
	select {
	case <-done:
		t.Fatal("beginStop returned with a request in flight")
	case <-time.After(60 * time.Millisecond):
	}
	d.endRequest()
	if !<-done {
		t.Fatal("beginStop did not see the daemon go idle")
	}

	d2 := newDaemon(&fakeDialer{}, defaultLifecycle())
	d2.tryBegin()
	start := time.Now()
	if d2.beginStop(80*time.Millisecond) || time.Since(start) < 70*time.Millisecond {
		t.Fatal("the cap was not honoured")
	}
}

func TestStoppingRefusalHasAMachineCheckableCode(t *testing.T) {
	r := errorResponse(errStopping)
	if r.OK || r.Code != "stopping" {
		t.Fatalf("%+v", r)
	}
}
