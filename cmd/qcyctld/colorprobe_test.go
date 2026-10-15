package main

import (
	"os"
	"path/filepath"
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

// An address-only cache entry (no vendorId) gets one short scan; its result
// reaches the cache and the model detection.
func TestProbeAdvertFillsVendorFromAddressOnlyCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var addr bluetooth.Address
	addr.Set("AA:BB:CC:DD:EE:FF")
	if err := os.MkdirAll(filepath.Dir(cacheFilePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheFilePath(), []byte("AA:BB:CC:DD:EE:FF 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ce, _ := loadCacheEntry()
	calls := fakeScan(t, 23877, 2, true)
	b := &bleDialer{}
	v, hv, c, hc := b.probeAdvert(nil, addr, ce)
	if *calls != 1 || !hv || v != 23877 || !hc || c != 2 {
		t.Fatalf("probe: %d %v %d %v calls=%d", v, hv, c, hc, *calls)
	}
	saveCachedAddr(addr, ce.Name, v, hv, c, hc)
	b.setVendor(v, hv)
	b.setColor(c, hc)
	if e, _ := loadCacheEntry(); !e.HaveVendor || e.VendorID != 23877 || !e.HaveColor || e.ColorIndex != 2 {
		t.Fatalf("cache: %+v", e)
	}
	if got, ok := b.VendorID(); !ok || got != 23877 {
		t.Fatal("dialer does not report the vendor")
	}
	if detectModel(v, hv, "", "") == "generic" {
		t.Fatal("model still generic with the vendorId")
	}

	// A miss keeps the fast path as before, and is bounded.
	calls = fakeScan(t, 0, 0, false)
	b = &bleDialer{}
	if _, hv, _, _ := b.probeAdvert(nil, addr, cacheEntry{Addr: "x"}); hv {
		t.Fatal("a miss produced a vendor")
	}
	b.probeAdvert(nil, addr, cacheEntry{Addr: "x"})
	if *calls != 1 {
		t.Fatalf("calls=%d, want 1 inside the interval", *calls)
	}
}
