package main

import (
	"testing"
	"time"
)

func TestWaitProbeBlocksUntilSweepEnds(t *testing.T) {
	d := &daemon{}
	d.probing = true
	go func() {
		time.Sleep(150 * time.Millisecond)
		d.mu.Lock()
		d.probing = false
		d.mu.Unlock()
	}()
	start := time.Now()
	d.waitProbe(2 * time.Second)
	if el := time.Since(start); el < 100*time.Millisecond || el > time.Second {
		t.Fatalf("waitProbe returned after %v, want ~150ms", el)
	}
}

func TestWaitProbeIsBounded(t *testing.T) {
	d := &daemon{}
	d.probing = true
	start := time.Now()
	d.waitProbe(120 * time.Millisecond)
	if el := time.Since(start); el > time.Second {
		t.Fatalf("waitProbe not bounded: %v", el)
	}
}
