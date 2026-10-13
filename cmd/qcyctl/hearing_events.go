package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// `qcyctl hearing run --events`: the same engine and the same guards as the
// terminal run, driven by a program (the plugin). stdout carries one JSON
// object per line, stdin carries one command per line, the human progress text
// goes to stderr. Nothing is ever written to the earbuds in this mode: the
// result is saved to --out and applied later with `hearing apply`.

// eventKeys turns stdin command lines into the keys of the terminal run:
// "heard" = h, "not-heard" = n, "repeat" = r, "abort" = q. Anything else is
// ignored. EOF is io.EOF (the controller went away: abort).
type eventKeys struct{ sc *bufio.Scanner }

func newEventKeys(r io.Reader) *eventKeys { return &eventKeys{sc: bufio.NewScanner(r)} }

func (e *eventKeys) ReadKey() (byte, error) {
	for e.sc.Scan() {
		switch strings.ToLower(strings.TrimSpace(e.sc.Text())) {
		case "heard":
			return 'h', nil
		case "not-heard":
			return 'n', nil
		case "repeat":
			return 'r', nil
		case "abort":
			return 'q', nil
		}
	}
	if err := e.sc.Err(); err != nil {
		return 0, err
	}
	return 0, io.EOF
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

// defaultHearingOut is ~/.cache/qcy-omarchy/hearing-<ts>.json (the directory
// is created 0700).
func defaultHearingOut(now time.Time) (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "qcy-omarchy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "hearing-"+now.Format("20060102-150405")+".json"), nil
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
