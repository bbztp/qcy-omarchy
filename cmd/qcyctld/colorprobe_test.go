package main

import (
	"testing"
	"time"

	"tinygo.org/x/bluetooth"
)

func fakeScan(t *testing.T, v uint16, c uint8, ok bool) *int {
	t.Helper()
	calls := 0
	old := advertScan
	advertScan = func(*bluetooth.Adapter, bluetooth.Address, time.Duration) (uint16, uint8, bool) {
		calls++
		return v, c, ok
	}
	t.Cleanup(func() { advertScan = old })
	return &calls
}

func TestProbeColor(t *testing.T) {
	old := cacheEntry{Addr: "AA:BB:CC:DD:EE:FF", VendorID: 23872, HaveVendor: true}

	calls := fakeScan(t, 23872, 2, true)
	b := &bleDialer{}
	if c, ok := b.probeColor(nil, bluetooth.Address{}, old); !ok || c != 2 || *calls != 1 {
		t.Fatalf("cache without colour: scan expected, got %d,%v calls=%d", c, ok, *calls)
	}

	// cache with colour: no scan
	*calls = 0
	withColor := old
	withColor.ColorIndex, withColor.HaveColor = 5, true
	if c, ok := (&bleDialer{}).probeColor(nil, bluetooth.Address{}, withColor); !ok || c != 5 || *calls != 0 {
		t.Fatalf("cache with colour: %d,%v calls=%d", c, ok, *calls)
	}
	// no vendor: no scan
	if _, ok := (&bleDialer{}).probeColor(nil, bluetooth.Address{}, cacheEntry{Addr: "x"}); ok || *calls != 0 {
		t.Fatal("no vendor must not scan")
	}

	// a different vendor on the air is not stored
	fakeScan(t, 1, 3, true)
	if _, ok := (&bleDialer{}).probeColor(nil, bluetooth.Address{}, old); ok {
		t.Fatal("vendor mismatch must not yield a colour")
	}

	// a miss is bounded: one try per interval, at most colorProbeMaxTries
	calls = fakeScan(t, 0, 0, false)
	b = &bleDialer{}
	b.probeColor(nil, bluetooth.Address{}, old)
	b.probeColor(nil, bluetooth.Address{}, old)
	if *calls != 1 {
		t.Fatalf("second try inside the interval must not scan, calls=%d", *calls)
	}
	for i := 0; i < 5; i++ {
		b.lastColorTry = time.Now().Add(-time.Hour)
		b.probeColor(nil, bluetooth.Address{}, old)
	}
	if *calls != colorProbeMaxTries {
		t.Fatalf("calls=%d, want cap %d", *calls, colorProbeMaxTries)
	}
}
