package main

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeSocketDaemon serves handler(n, line, conn) for each request line, where n
// counts requests across connections; it returns the address.
func fakeSocketDaemon(t *testing.T, handler func(n int, conn net.Conn)) (string, *int, *sync.Mutex) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	n := 0
	var mu sync.Mutex
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					if _, err := r.ReadString('\n'); err != nil {
						return
					}
					mu.Lock()
					n++
					k := n
					mu.Unlock()
					handler(k, c)
				}
			}()
		}
	}()
	return sock, &n, &mu
}

func clientFor(t *testing.T, sock string) *daemonClient {
	t.Helper()
	dial := func() (net.Conn, error) { return net.DialTimeout("unix", sock, time.Second) }
	c, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	dc := &daemonClient{redial: dial}
	dc.setConn(c)
	return dc
}

func noSleep(t *testing.T) *time.Duration {
	var total time.Duration
	old := retrySleep
	retrySleep = func(d time.Duration) { total += d }
	t.Cleanup(func() { retrySleep = old })
	return &total
}

func TestRequestRetriesAfterStoppingRefusal(t *testing.T) {
	slept := noSleep(t)
	sock, n, mu := fakeSocketDaemon(t, func(k int, c net.Conn) {
		if k == 1 {
			json.NewEncoder(c).Encode(map[string]interface{}{"ok": false, "error": "daemon stopping", "code": "stopping"})
			return
		}
		json.NewEncoder(c).Encode(map[string]interface{}{"ok": true, "data": "fine"})
	})
	dc := clientFor(t, sock)
	data, err := dc.request("status", nil)
	if err != nil || data != "fine" {
		t.Fatalf("got %v, %v", data, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if *n != 2 || *slept == 0 {
		t.Fatalf("requests=%d slept=%v, want exactly one retry with a backoff", *n, *slept)
	}
}

func TestRequestDoesNotRetryAfterTheConnectionDropsMidReply(t *testing.T) {
	noSleep(t)
	sock, n, mu := fakeSocketDaemon(t, func(k int, c net.Conn) {
		c.Close() // sent, then lost: it may have executed
	})
	dc := clientFor(t, sock)
	if _, err := dc.request("set", nil); err == nil {
		t.Fatal("want an error")
	}
	mu.Lock()
	defer mu.Unlock()
	if *n != 1 {
		t.Fatalf("a sent request was retried (%d requests)", *n)
	}
}

func TestRequestGivesUpAfterTheBudget(t *testing.T) {
	slept := noSleep(t)
	sock, n, mu := fakeSocketDaemon(t, func(k int, c net.Conn) {
		json.NewEncoder(c).Encode(map[string]interface{}{"ok": false, "error": "daemon stopping", "code": "stopping"})
	})
	dc := clientFor(t, sock)
	_, err := dc.request("status", nil)
	if err == nil || err.Error() != "daemon stopping" {
		t.Fatalf("err = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if *slept < restartRetryBudget || *slept > restartRetryBudget+3*time.Second || *n < 5 {
		t.Fatalf("slept %v over %d requests", *slept, *n)
	}
}

func TestRequestReconnectsWhenTheSocketIsBrieflyGone(t *testing.T) {
	noSleep(t)
	sock, _, _ := fakeSocketDaemon(t, func(k int, c net.Conn) {
		json.NewEncoder(c).Encode(map[string]interface{}{"ok": true, "data": "up"})
	})
	dc := clientFor(t, sock)
	dc.conn.Close() // the old daemon's connection died before the send
	fails := 2
	good := dc.redial
	dc.redial = func() (net.Conn, error) {
		if fails > 0 {
			fails--
			return nil, net.ErrClosed
		}
		return good()
	}
	data, err := dc.request("status", nil)
	if err != nil || data != "up" {
		t.Fatalf("got %v, %v", data, err)
	}
}

func TestRequestReturnsOtherErrorCodesAtOnce(t *testing.T) {
	slept := noSleep(t)
	sock, n, mu := fakeSocketDaemon(t, func(k int, c net.Conn) {
		json.NewEncoder(c).Encode(map[string]interface{}{"ok": false, "error": "earbuds busy", "code": "busy"})
	})
	dc := clientFor(t, sock)
	if _, err := dc.request("status", nil); err == nil || err.Error() != "earbuds busy" {
		t.Fatalf("err = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if *n != 1 || *slept != 0 {
		t.Fatalf("requests=%d slept=%v, want no retry", *n, *slept)
	}
}

func TestRequestRetriesAnEncodeFailureOnAHalfDeadConnection(t *testing.T) {
	noSleep(t)
	var lines int
	var lmu sync.Mutex
	sock := filepath.Join(t.TempDir(), "live.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if len(line) > 1 && line[len(line)-1] == '\n' {
						lmu.Lock()
						lines++
						lmu.Unlock()
					}
					json.NewEncoder(c).Encode(map[string]interface{}{"ok": true, "data": "ok"})
				}
			}()
		}
	}()
	// First connection: the peer closes it before the client writes.
	dsock := filepath.Join(t.TempDir(), "dead.sock")
	dl, err := net.Listen("unix", dsock)
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Close()
	go func() {
		c, err := dl.Accept()
		if err == nil {
			c.Close()
		}
	}()
	dead, err := net.Dial("unix", dsock)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the close reach us
	dc := &daemonClient{redial: func() (net.Conn, error) { return net.Dial("unix", sock) }}
	dc.setConn(dead)
	data, err := dc.request("set", map[string]interface{}{"x": 1})
	if err != nil || data != "ok" {
		t.Fatalf("got %v, %v", data, err)
	}
	lmu.Lock()
	defer lmu.Unlock()
	if lines != 1 {
		t.Fatalf("the server received the request line %d times, want exactly once", lines)
	}
}
