package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

func parsePct(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	if err != nil || n < 1 || n > 100 {
		return 0, fmt.Errorf("%q must be 1%%..100%%", s)
	}
	return n, nil
}

func cmdFind(args []string) error { return findMain(args, os.Stdout) }

func findMain(args []string, out io.Writer) error {
	o, events, err := parseFindArgs(args)
	if err != nil {
		return err
	}
	env, done := liveFindEnv(out, events)
	defer done()
	return findRunLive(env, o)
}

func parseFindArgs(args []string) (findOpts, bool, error) {
	fs := flag.NewFlagSet("find", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sink := fs.String("sink", "", "")
	maxVol := fs.String("max-volume", fmt.Sprintf("%d%%", findDefaultMax), "")
	startVol := fs.String("start-volume", "", "")
	toneLevel := fs.Float64("tone-level", 0, "")
	timeout := fs.Int("timeout", findDefaultTime, "")
	yes := fs.Bool("yes", false, "")
	events := fs.Bool("events", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return findOpts{}, false, errors.New("usage: qcyctl find [--sink NAME] [--start-volume 100%] [--tone-level -3] [--max-volume 100%] [--timeout 60] [--yes] [--events]")
	}
	pct, err := parsePct(*maxVol)
	if err != nil {
		return findOpts{}, false, fmt.Errorf("--max-volume: %w", err)
	}
	startPct := 0
	if *startVol != "" {
		if startPct, err = parsePct(*startVol); err != nil {
			return findOpts{}, false, fmt.Errorf("--start-volume: %w", err)
		}
	}
	if *toneLevel != 0 {
		if *toneLevel < -30 || *toneLevel > -3 {
			return findOpts{}, false, errors.New("--tone-level: -30..-3 dBFS")
		}
		findBeepAmp = math.Pow(10, *toneLevel/20)
	}
	if *timeout < 5 || *timeout > 600 {
		return findOpts{}, false, errors.New("--timeout: 5..600 seconds")
	}
	if *events && !*yes {
		return findOpts{}, false, errors.New("find --events has no terminal to confirm on: pass --yes after showing the warning: " + findWarning)
	}
	return findOpts{sinkName: *sink, maxPct: pct, startPct: startPct, timeout: time.Duration(*timeout) * time.Second, yes: *yes}, *events, nil
}
