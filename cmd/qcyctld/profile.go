package main

import (
	"fmt"
	"strconv"
	"strings"

	quicky "github.com/hui1601/Quicky/lib"

	"github.com/bbztp/qcy-omarchy/internal/qcycore/models"
)

// quickyProduct resolves a vendorId against the Quicky product database. The
// library only exposes the lookup through ScanResult, so a synthetic one is used.
func quickyProduct(vendorID uint16) (*quicky.Product, bool) {
	r := quicky.ScanResult{Advertisement: &quicky.AdvertisementInfo{VendorID: vendorID}}
	return r.GetProductInfo()
}

// lookupProduct answers "is this vendorId a known QCY product" for detectModel:
// the Quicky database first, then the embedded snapshot (as an entry that
// declares nothing; the snapshot itself is read by newProfileHandler).
func lookupProduct(vendorID uint16) (*quicky.Product, bool) {
	if p, ok := quickyProduct(vendorID); ok {
		return p, true
	}
	if sp, ok := models.Lookup(vendorID); ok {
		return &quicky.Product{Title: sp.Title}, true
	}
	return nil, false
}

// vendorFromModelID parses "qcy-<vendorId>".
func vendorFromModelID(id string) (uint16, bool) {
	rest, ok := strings.CutPrefix(id, "qcy-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(rest, 10, 16)
	if err != nil {
		return 0, false
	}
	return uint16(n), true
}

// profileHandler drives a QCY model that is in the product database but was
// never exercised here. It offers only the shared command set (the
// GenericHandler's actions) and only the part of it the product declares. It
// never writes EQ and never invents opcodes.
type profileHandler struct {
	vendorID uint16
	title    string
	caps     Capabilities
}

// newProfileHandler prefers the embedded snapshot (what the QCY server
// declares for the vendorId) and falls back to the Quicky database when the
// vendorId is not in the snapshot or the snapshot has no panel for it.
func newProfileHandler(vendorID uint16) (*profileHandler, bool) {
	sp, inSnap := models.Lookup(vendorID)
	if inSnap && sp.Panel != nil {
		return &profileHandler{vendorID: vendorID, title: sp.Title, caps: snapshotCapabilities(vendorID, sp)}, true
	}
	p, ok := quickyProduct(vendorID)
	if !ok {
		if inSnap {
			// Listed by QCY but no panel and no database entry: nothing is
			// declared, only the battery readout.
			return &profileHandler{vendorID: vendorID, title: sp.Title, caps: baseProfileCaps(vendorID, sp.Title, nil, nil)}, true
		}
		return nil, false
	}
	caps := profileCapabilities(vendorID, p)
	if inSnap && sp.Title != "" {
		caps.DisplayName, caps.ProductTitle = sp.Title, sp.Title
	}
	return &profileHandler{vendorID: vendorID, title: caps.ProductTitle, caps: caps}, true
}

// featureOfAction maps the shared action ids to their feature flags.
var featureOfAction = map[string]FeatureFlag{
	"anc": FeatureANC, "gameMode": FeatureGameMode, "sleepMode": FeatureSleepMode,
	"ldac": FeatureLDAC, "multipoint": FeatureMultipoint, "windNoise": FeatureWindNoise,
	"wearDetect": FeatureWearDetect, "autoOff": FeatureAutoOff, "balance": FeatureBalance,
}

// toggleByCmd maps the command id of a panel toggle (layout type 4, or the
// dedicated gaming/multi-device layouts) onto the shared action it drives.
var toggleByCmd = map[int]string{9: "gameMode", 16: "sleepMode", 35: "ldac", 36: "multipoint", 42: "windNoise", 44: "wearDetect"}

// snapshotCapabilities derives the offer from the server panel. Only the
// shared command set is offered, filtered by what the panel declares: EQ, fit,
// prompt volume and wear sensitivity are never offered on an unverified model
// even though the panel lists them.
func snapshotCapabilities(vendorID uint16, p models.Product) Capabilities {
	pn := p.Panel
	declared := map[string]bool{}
	if pn.ANC != nil {
		declared["anc"] = true
	}
	if pn.Balance {
		declared["balance"] = true
	}
	if pn.AutoOff != nil {
		declared["autoOff"] = true
	}
	if pn.Wear != nil {
		declared["wearDetect"] = true
	}
	for _, o := range pn.Other {
		if o.Type == nil || o.CmdID == nil {
			continue
		}
		if *o.Type == 4 || *o.Type == 113 {
			if id, ok := toggleByCmd[*o.CmdID]; ok {
				declared[id] = true
			}
		}
	}
	return baseProfileCaps(vendorID, p.Title, declared, &pn.Find)
}

// baseProfileCaps builds the capabilities from the set of declared shared
// action ids (find is a feature flag, not an action).
func baseProfileCaps(vendorID uint16, title string, declared map[string]bool, find *bool) Capabilities {
	common := GenericHandler{}.Capabilities()
	var actions []ActionSpec
	var features []FeatureFlag
	for _, a := range common.Actions {
		if declared[a.ID] {
			actions = append(actions, a)
			features = append(features, featureOfAction[a.ID])
		}
	}
	if find != nil && *find {
		features = append(features, FeatureFind)
	}
	features = append(features, FeatureBatteryCase)
	return Capabilities{
		ModelID:      "qcy-" + strconv.Itoa(int(vendorID)),
		DisplayName:  title,
		Features:     features,
		Actions:      actions,
		BatteryChars: common.BatteryChars,
		Verified:     false,
		VendorID:     int(vendorID),
		ProductTitle: title,
	}
}

// declaredSettings returns the handler action ids the product declares
// through its settings (by command id, or by name when the id is absent).
func declaredSettings(p *quicky.Product) map[string]bool {
	out := map[string]bool{}
	for _, s := range p.Features.Settings {
		n := strings.ToLower(s.Name)
		id := -1
		if s.CmdID != nil {
			id = *s.CmdID
		}
		switch {
		case id == 9 || strings.Contains(n, "gaming") || strings.Contains(n, "low-latency"):
			out["gameMode"] = true
		case id == 16 || strings.Contains(n, "sleep"):
			out["sleepMode"] = true
		case id == 42 || strings.Contains(n, "wind"):
			out["windNoise"] = true
		case id == 35 || strings.Contains(n, "ldac"):
			out["ldac"] = true
		case id == 36 || strings.Contains(n, "dual device") || strings.Contains(n, "multi-device"):
			out["multipoint"] = true
		case id == 44 || strings.Contains(n, "in-ear") || strings.Contains(n, "wear"):
			out["wearDetect"] = true
		}
	}
	return out
}

func profileCapabilities(vendorID uint16, p *quicky.Product) Capabilities {
	declared := declaredSettings(p)
	if p.Features.ANC != nil {
		declared["anc"] = true
	}
	if p.Features.ChannelBalance {
		declared["balance"] = true
	}
	if p.Features.AutoOffTimer != nil {
		declared["autoOff"] = true
	}

	featureOf := map[string]FeatureFlag{
		"anc": FeatureANC, "gameMode": FeatureGameMode, "sleepMode": FeatureSleepMode,
		"ldac": FeatureLDAC, "multipoint": FeatureMultipoint, "windNoise": FeatureWindNoise,
		"wearDetect": FeatureWearDetect, "autoOff": FeatureAutoOff, "balance": FeatureBalance,
	}
	common := GenericHandler{}.Capabilities()
	var actions []ActionSpec
	var features []FeatureFlag
	for _, a := range common.Actions {
		if declared[a.ID] {
			actions = append(actions, a)
			features = append(features, featureOf[a.ID])
		}
	}
	if p.Features.FindEarphone {
		features = append(features, FeatureFind)
	}
	features = append(features, FeatureBatteryCase)
	// Features.EQ is deliberately not mapped: EQ stays unavailable on models
	// that were not verified, so no write path is advertised.
	return Capabilities{
		ModelID:      "qcy-" + strconv.Itoa(int(vendorID)),
		DisplayName:  p.Title,
		Features:     features,
		Actions:      actions,
		BatteryChars: common.BatteryChars,
		Verified:     false,
		VendorID:     int(vendorID),
		ProductTitle: p.Title,
	}
}

func (h *profileHandler) hasFeature(f FeatureFlag) bool {
	for _, x := range h.caps.Features {
		if x == f {
			return true
		}
	}
	return false
}

func (h *profileHandler) Capabilities() Capabilities { return h.caps }

func (h *profileHandler) HandleSet(d *daemon, actionID string, params, expect []byte) ([]byte, error) {
	// Only what the product declared. find is a feature flag, not an action;
	// the read-only commands go on to the daemon's own guard and its reason.
	want := daemonActionID(actionID)
	declared := false
	for _, a := range h.caps.Actions {
		if daemonActionID(a.ID) == want {
			declared = true
		}
	}
	if _, ro := readOnlyCmds[want]; !ro && !declared &&
		!(want == "find" && h.hasFeature(FeatureFind)) {
		return nil, fmt.Errorf("%s is not declared by %s", actionID, h.title)
	}
	return d.setAction(daemonActionID(actionID), params, expect)
}

func (h *profileHandler) HandleRead(d *daemon, actionID string) (interface{}, error) {
	return nil, fmt.Errorf("profile handler: unknown read %s", actionID)
}

func (h *profileHandler) OnConnect(d *daemon) error { return nil }
func (h *profileHandler) OnDisconnect(d *daemon)    {}
