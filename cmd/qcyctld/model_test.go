package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseCacheEntry(t *testing.T) {
	cases := []struct {
		name, in string
		want     cacheEntry
		ok       bool
	}{
		{"addr only", "AA:BB:CC:DD:EE:FF\n", cacheEntry{Addr: "AA:BB:CC:DD:EE:FF"}, true},
		{"old two fields", "AA:BB:CC:DD:EE:FF 1\n", cacheEntry{Addr: "AA:BB:CC:DD:EE:FF", Random: true}, true},
		{"old three fields", "AA:BB:CC:DD:EE:FF 0 QCY_MeloBuds_N70\n",
			cacheEntry{Addr: "AA:BB:CC:DD:EE:FF", Name: "QCY MeloBuds N70"}, true},
		{"new four fields", "AA:BB:CC:DD:EE:FF 1 QCY_MeloBuds_N70 23872\n",
			cacheEntry{Addr: "AA:BB:CC:DD:EE:FF", Random: true, Name: "QCY MeloBuds N70", VendorID: 23872, HaveVendor: true}, true},
		{"vendor without name", "AA:BB:CC:DD:EE:FF 0 - 19786\n",
			cacheEntry{Addr: "AA:BB:CC:DD:EE:FF", VendorID: 19786, HaveVendor: true}, true},
		{"junk vendor ignored", "AA:BB:CC:DD:EE:FF 0 x notanumber\n", cacheEntry{Addr: "AA:BB:CC:DD:EE:FF", Name: "x"}, true},
		{"empty", "  \n", cacheEntry{}, false},
	}
	for _, c := range cases {
		got, ok := parseCacheEntry(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got %+v,%v want %+v,%v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestFormatCacheEntryRoundTrip(t *testing.T) {
	old := cacheEntry{Addr: "AA:BB:CC:DD:EE:FF", Random: true, Name: "QCY MeloBuds N70"}
	if got := formatCacheEntry(old); got != "AA:BB:CC:DD:EE:FF 1 QCY_MeloBuds_N70\n" {
		t.Fatalf("no vendor must keep the old 3-field layout, got %q", got)
	}
	for _, e := range []cacheEntry{
		old,
		{Addr: "AA:BB:CC:DD:EE:FF", Name: "QCY MeloBuds N70", VendorID: 23872, HaveVendor: true},
		{Addr: "AA:BB:CC:DD:EE:FF", VendorID: 19786, HaveVendor: true},
	} {
		back, ok := parseCacheEntry(formatCacheEntry(e))
		if !ok || back != e {
			t.Errorf("round trip %+v -> %+v", e, back)
		}
	}
}

func TestDetectModelPrecedence(t *testing.T) {
	cases := []struct {
		name   string
		vendor uint16
		have   bool
		dev    string
		fw     string
		want   string
	}{
		{"n70 first colour", 23872, true, "", "", "melobuds-n70"},
		{"n70 last colour", 23877, true, "whatever", "", "melobuds-n70"},
		// Neighbouring ids are other products, never the N70.
		{"23873 AilyBuds E50", 23873, true, "", "", "qcy-23873"},
		{"23874 MeloBuds N60", 23874, true, "", "", "qcy-23874"},
		{"23875 CT03", 23875, true, "", "", "qcy-23875"},
		{"23876 not in the database", 23876, true, "", "", "generic"},
		{"known non-n70", 19786, true, "", "", "qcy-19786"},
		{"vendor beats an n70-looking name", 19786, true, "QCY MeloBuds N70", "", "qcy-19786"},
		{"unknown vendor falls to name", 1, true, "QCY MeloBuds N70", "", "melobuds-n70"},
		{"unknown vendor, unknown name", 1, true, "Foo", "", "generic"},
		{"no vendor uses name", 0, false, "QCY MeloBuds N70", "", "melobuds-n70"},
		{"vendor 0 is not a vendor when absent", 19786, false, "", "", "generic"},
		{"firmware string last", 0, false, "", "melobuds", "melobuds-n70"},
	}
	for _, c := range cases {
		if got := detectModel(c.vendor, c.have, c.dev, c.fw); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestProfileHandlerMatchesProduct(t *testing.T) {
	// 19786 QCY MeloBuds Pro: anc, eq, channel_balance, find_earphone,
	// auto_off_timer; settings Gaming mode, Sleeping Mode, LDAC Switch,
	// Dual Device Connection.
	h := GetHandler("qcy-19786")
	c := h.Capabilities()
	if _, ok := h.(*profileHandler); !ok {
		t.Fatalf("handler is %T", h)
	}
	if c.ModelID != "qcy-19786" || c.DisplayName != "QCY MeloBuds Pro" || c.ProductTitle != "QCY MeloBuds Pro" ||
		c.VendorID != 19786 || c.Verified {
		t.Fatalf("caps %+v", c)
	}
	ids := map[string]bool{}
	for _, a := range c.Actions {
		ids[a.ID] = true
	}
	// The server panel of 19786 also lists wear detection (type 104).
	for _, id := range []string{"anc", "balance", "autoOff", "gameMode", "sleepMode", "ldac", "multipoint", "wearDetect"} {
		if !ids[id] {
			t.Errorf("action %s missing", id)
		}
	}
	for _, id := range []string{"windNoise", "promptVolume", "wearSensitivity"} {
		if ids[id] {
			t.Errorf("action %s must not be offered (product does not declare it)", id)
		}
	}
	feats := map[FeatureFlag]bool{}
	for _, f := range c.Features {
		feats[f] = true
	}
	if feats[FeatureEQ] || feats[FeatureKeysRemap] || feats[FeatureFit] {
		t.Error("EQ/keys/fit must not be offered on an unverified model")
	}
	if !feats[FeatureFind] || !feats[FeatureANC] || !feats[FeatureBalance] || !feats[FeatureAutoOff] {
		t.Errorf("features %v", c.Features)
	}
	// Every action is a copy of the common set, never an invented opcode.
	common := map[string]byte{}
	for _, a := range (GenericHandler{}).Capabilities().Actions {
		common[a.ID] = a.Opcode
	}
	for _, a := range c.Actions {
		if op, ok := common[a.ID]; !ok || op != a.Opcode {
			t.Errorf("action %s opcode 0x%02x not in the common set", a.ID, a.Opcode)
		}
	}
}

func TestGetHandlerUnknownVendorIDIsGeneric(t *testing.T) {
	if _, ok := GetHandler("qcy-1").(GenericHandler); !ok {
		t.Fatal("unknown vendor id must fall back to the generic handler")
	}
	if _, ok := GetHandler("qcy-notanumber").(GenericHandler); !ok {
		t.Fatal("malformed id must fall back to the generic handler")
	}
	if !GetHandler("melobuds-n70").Capabilities().Verified {
		t.Fatal("n70 must be verified")
	}
	if GetHandler("generic").Capabilities().Verified {
		t.Fatal("generic must not be verified")
	}
}

// vendorDialer is a fakeDialer that also reports a vendorId, like the BLE dialer.
type vendorDialer struct {
	*fakeDialer
	vendor uint16
}

func (v vendorDialer) VendorID() (uint16, bool) { return v.vendor, true }

func TestDaemonUsesVendorIDAndKeepsProfileReadOnly(t *testing.T) {
	cfg := defaultLifecycle()
	cfg.idle = time.Hour
	d := newDaemon(vendorDialer{&fakeDialer{}, 19786}, cfg)
	rep := mustStatus(t, d)
	settle(t, d)

	m, ok := rep["model"].(map[string]interface{})
	if !ok {
		t.Fatalf("status has no model: %v", rep)
	}
	if m["modelId"] != "qcy-19786" || m["verified"] != false || m["vendorId"] != 19786 || m["productTitle"] != "QCY MeloBuds Pro" {
		t.Fatalf("model %v", m)
	}
	if c, _ := mustStatus(t, d)["capabilities"].(map[string]interface{}); c["eq"] != false {
		t.Errorf("eq must be false on an unverified model: %v", c)
	}

	for _, cmd := range []string{"eqApply", "eqMasterGain", "keysSet", "hearingMode", "hearingWrite"} {
		_, err := d.handleRequest(request{Cmd: cmd, Args: map[string]interface{}{"masterGain": float64(0)}})
		if err == nil || !strings.Contains(err.Error(), "not a verified model") {
			t.Errorf("%s: %v", cmd, err)
		}
	}
	// An action the product does not declare is refused with a clear error.
	_, err := d.handleRequest(request{Cmd: "set", Args: map[string]interface{}{"action": "windNoise"}})
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Errorf("windNoise: %v", err)
	}
	_, err = d.handleRequest(request{Cmd: "set", Args: map[string]interface{}{"action": "wind"}})
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Errorf("wind (daemon id): %v", err)
	}
	// promptVol and wearSensitivity are N70-only: a profile does not declare them.
	for _, a := range []string{"promptVol", "wearSensitivity"} {
		_, err = d.handleRequest(request{Cmd: "set", Args: map[string]interface{}{"action": a, "params": []interface{}{float64(6)}}})
		if err == nil || !strings.Contains(err.Error(), "not declared") {
			t.Errorf("%s: %v", a, err)
		}
	}
	if _, err = d.handleRequest(request{Cmd: "fitTest"}); err == nil || !strings.Contains(err.Error(), "not a verified model") {
		t.Errorf("fitTest: %v", err)
	}
	_, err = d.handleRequest(request{Cmd: "set", Args: map[string]interface{}{"action": "ldac"}})
	if err == nil || !strings.Contains(err.Error(), "verified model") {
		t.Errorf("ldac: %v", err)
	}
	if acts, _ := m["actions"].([]string); len(acts) == 0 {
		t.Errorf("model.actions missing: %v", m)
	}
	// ldac (verified models only) and promptVol are not writable here.
	for _, a := range []string{"ldac", "promptVol"} {
		_, err := d.handleRequest(request{Cmd: "set", Args: map[string]interface{}{"action": a}})
		if err == nil {
			t.Errorf("%s was writable", a)
		}
	}
	if _, ro := readOnlyCmds["ldac"]; ro {
		t.Error("ldac must not be in readOnlyCmds any more (verified-only instead)")
	}
	if !verifiedOnlyActions["ldac"] {
		t.Error("ldac must be a verified-only action")
	}
	if _, ro := readOnlyCmds["promptVol"]; ro == promptVolWritable {
		t.Errorf("promptVol read-only=%v with promptVolWritable=%v", ro, promptVolWritable)
	}
}

func TestDaemonN70ByVendorIDIsVerified(t *testing.T) {
	cfg := defaultLifecycle()
	cfg.idle = time.Hour
	d := newDaemon(vendorDialer{&fakeDialer{}, 23877}, cfg)
	rep := mustStatus(t, d)
	m := rep["model"].(map[string]interface{})
	if m["modelId"] != "melobuds-n70" || m["verified"] != true || m["vendorId"] != 23877 {
		t.Fatalf("model %v", m)
	}
}

// switchDialer lets one daemon see a different vendorId on each connection.
type switchDialer struct {
	*fakeDialer
	vendor *uint16
}

func (v switchDialer) VendorID() (uint16, bool) { return *v.vendor, true }

func statusEQ(t *testing.T, d *daemon) interface{} {
	t.Helper()
	mustStatus(t, d)
	settle(t, d)
	c, _ := mustStatus(t, d)["capabilities"].(map[string]interface{})
	return c["eq"]
}

func TestEQCapabilityFollowsTheConnectedModel(t *testing.T) {
	for _, c := range []struct {
		name        string
		first, next uint16
		wantFirst   bool
		wantNext    bool
	}{
		{"non-n70 then n70", 19786, 23872, false, true},
		{"n70 then non-n70", 23872, 19786, true, false},
	} {
		v := c.first
		cfg := defaultLifecycle()
		cfg.idle = time.Hour
		d := newDaemon(switchDialer{&fakeDialer{}, &v}, cfg)
		if got := statusEQ(t, d); got != c.wantFirst {
			t.Fatalf("%s: first eq=%v", c.name, got)
		}
		if _, err := d.handleRequest(request{Cmd: "disconnect"}); err != nil {
			t.Fatal(err)
		}
		v = c.next
		if got := statusEQ(t, d); got != c.wantNext {
			t.Fatalf("%s: after switch eq=%v", c.name, got)
		}
	}
}

func TestNeighbourVendorIDsAreNotVerified(t *testing.T) {
	for _, v := range []uint16{23873, 23874, 23875} {
		c := GetHandler(detectModel(v, true, "", "")).Capabilities()
		if c.Verified {
			t.Errorf("%d verified", v)
		}
		for _, f := range c.Features {
			if f == FeatureEQ {
				t.Errorf("%d offers EQ", v)
			}
		}
	}
	if _, ok := GetHandler(detectModel(23876, true, "", "")).(GenericHandler); !ok {
		t.Error("23876 must be generic")
	}
}

// 23880 (MeloBuds N70i) is only in the embedded snapshot, not in Quicky. Its
// panel lists EQ, fit, beep volume and wear sensitivity: none may be offered.
func TestSnapshotOnlyVendorIsAnUnverifiedProfile(t *testing.T) {
	if _, ok := quickyProduct(23880); ok {
		t.Skip("23880 is now in the Quicky database; pick another snapshot-only vendor")
	}
	if got := detectModel(23880, true, "", ""); got != "qcy-23880" {
		t.Fatalf("detectModel = %q", got)
	}
	h := GetHandler("qcy-23880")
	if _, ok := h.(*profileHandler); !ok {
		t.Fatalf("handler is %T", h)
	}
	c := h.Capabilities()
	if c.Verified || c.VendorID != 23880 || c.ProductTitle != "QCY MeloBuds N70i" || c.DisplayName != c.ProductTitle {
		t.Fatalf("caps %+v", c)
	}
	feats := map[FeatureFlag]bool{}
	for _, f := range c.Features {
		feats[f] = true
	}
	if feats[FeatureEQ] || feats[FeatureFit] || feats[FeatureKeysRemap] {
		t.Errorf("EQ/fit/keys offered: %v", c.Features)
	}
	if !feats[FeatureANC] || !feats[FeatureWearDetect] || !feats[FeatureFind] {
		t.Errorf("features %v", c.Features)
	}
	for _, a := range c.Actions {
		switch a.ID {
		case "promptVolume", "wearSensitivity":
			t.Errorf("action %s must not be offered", a.ID)
		}
	}
	// The verified N70 is unchanged.
	if detectModel(23872, true, "", "") != "melobuds-n70" || detectModel(23877, true, "", "") != "melobuds-n70" {
		t.Error("n70 detection changed")
	}
	if !GetHandler("melobuds-n70").Capabilities().Verified {
		t.Error("n70 must stay verified")
	}
}

// 16755 (QCY H3) has no wear detection in its panel: the command is refused.
func TestProfileRefusesWearWhenPanelLacksIt(t *testing.T) {
	cfg := defaultLifecycle()
	cfg.idle = time.Hour
	d := newDaemon(vendorDialer{&fakeDialer{}, 16755}, cfg)
	mustStatus(t, d)
	settle(t, d)
	if _, err := d.handleRequest(request{Cmd: "wear", Args: map[string]interface{}{"state": float64(1)}}); err == nil ||
		!strings.Contains(err.Error(), "does not declare") {
		t.Errorf("wear: %v", err)
	}
}
