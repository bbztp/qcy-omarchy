package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// `qcyctl hearing run --events`: the same engine and the same guards as the
// terminal run, driven by a program (the plugin). stdout carries one JSON
// object per line, stdin carries one command per line, the human progress text
// goes to stderr. Nothing is ever written to the earbuds in this mode: the
// result is saved to --out and applied later with `hearing apply`.

// maxEventLine bounds one stdin command; a longer line is ignored (it does
// not abort the run).
const maxEventLine = 1 << 20

// eventKeys turns stdin command lines into the keys of the terminal run:
// "heard" = h, "not-heard" = n, "repeat" = r, "abort" = q. "heard N" and
// "not-heard N" carry the index of the tone they answer (LastIndex reports it
// for the key just read; 0 = none). Anything else is ignored. EOF is io.EOF
// (the controller went away: abort).
type eventKeys struct {
	r   *bufio.Reader
	idx int
}

func newEventKeys(r io.Reader) *eventKeys { return &eventKeys{r: bufio.NewReaderSize(r, 64<<10)} }

// LastIndex is the tone index given with the last key (0 when none).
func (e *eventKeys) LastIndex() int { return e.idx }

// line reads one line; ok is false for a line longer than maxEventLine.
func (e *eventKeys) line() (text string, ok bool, err error) {
	var buf []byte
	long := false
	for {
		chunk, prefix, err := e.r.ReadLine()
		if err != nil {
			return "", false, err
		}
		if !long {
			if len(buf)+len(chunk) > maxEventLine {
				long, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if !prefix {
			return string(buf), !long, nil
		}
	}
}

func (e *eventKeys) ReadKey() (byte, error) {
	for {
		text, ok, err := e.line()
		if err != nil {
			return 0, err // io.EOF included
		}
		if !ok {
			continue
		}
		f := strings.Fields(strings.ToLower(text))
		if len(f) == 0 || len(f) > 2 {
			continue
		}
		idx := 0
		if len(f) == 2 {
			n, err := strconv.Atoi(f[1])
			if err != nil || n < 1 || (f[0] != "heard" && f[0] != "not-heard") {
				continue
			}
			idx = n
		}
		var k byte
		switch f[0] {
		case "heard":
			k = 'h'
		case "not-heard":
			k = 'n'
		case "repeat":
			k = 'r'
		case "abort":
			k = 'q'
		default:
			continue
		}
		e.idx = idx
		return k, nil
	}
}

// newEventEmitter returns the writer of the event lines ("event" is the first
// key of every object; writes are serialised).
func newEventEmitter(w io.Writer) func(string, map[string]interface{}) {
	var mu sync.Mutex
	return func(name string, fields map[string]interface{}) {
		m := map[string]interface{}{"event": name}
		for k, v := range fields {
			m[k] = v
		}
		b, err := json.Marshal(m)
		if err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "%s\n", b)
	}
}

// finishEvents writes the closing events after the cleanup ran and maps the
// run's error: an abort (command or EOF) is a normal end (nil), a signal or a
// failure is returned.
func finishEvents(emit func(string, map[string]interface{}), err error) error {
	reason := "done"
	switch {
	case err == nil:
		emit("phase", map[string]interface{}{"name": "done", "message": "test finished"})
	case errors.Is(err, errStopRequested):
		reason, err = "abort", nil
		emit("phase", map[string]interface{}{"name": "aborted", "message": "aborted by the controller; nothing was written"})
	case errors.Is(err, errInterrupted):
		reason = "signal"
		emit("phase", map[string]interface{}{"name": "aborted", "message": "interrupted by a signal; nothing was written"})
	default:
		reason = "error"
		emit("error", map[string]interface{}{"message": err.Error()})
		emit("phase", map[string]interface{}{"name": "error", "message": err.Error()})
	}
	emit("stopped", map[string]interface{}{"reason": reason})
	return err
}

// defaultHearingOut creates ~/.cache/qcy-omarchy/hearing-<ts>.json (the
// directory is forced to 0700; the file is created exclusively, without
// following a symlink, mode 0600; a collision gets a numeric suffix) and
// returns its path.
func defaultHearingOut(now time.Time) (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "qcy-omarchy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	base := "hearing-" + now.Format("20060102-150405.000")
	for n := 0; n < 100; n++ {
		name := base + ".json"
		if n > 0 {
			name = fmt.Sprintf("%s-%d.json", base, n)
		}
		p := filepath.Join(dir, name)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return p, f.Close()
	}
	return "", errors.New("cannot create a result file in " + dir)
}

// liveHearingEventsEnv is liveHearingEnv with stdin commands instead of a
// terminal and the progress text on stderr.
func liveHearingEventsEnv() (hearingEnv, func()) {
	env, done := liveHearingEnv(os.Stderr)
	env.keys = func() (keySource, func(), error) {
		if err := refuseHardware(); err != nil {
			return nil, nil, err
		}
		return newEventKeys(os.Stdin), func() {}, nil
	}
	return env, done
}
