package main

import (
	"fmt"
	"sort"
	"sync"

	"github.com/bbztp/qcy-omarchy/internal/protocol"
	"github.com/bbztp/qcy-omarchy/internal/qcycore/models"
)

// What a model offers is what its embedded server panel declares AND this tool
// understands the encoding of. A feature whose encoding is not understood is
// simply not declared (and a note says why); "not tested on hardware" is never
// a reason to withhold one (Capabilities.Verified is informational).

// NamedValue is a labelled value of a selector (wear sensitivity).
type NamedValue struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

// ANCItemSpec is one scene of an ANC group. Start and End are the packed
// values ((mode<<16)|(scene<<8)|level) the option covers; Params is Default
// (or Start) split into the three bytes the 0x17 write carries.
type ANCItemSpec struct {
	Name   string `json:"name"`
	Start  int    `json:"start"`
	End    int    `json:"end"`
	Params []int  `json:"params"`
}

// ANCModeSpec is one ANC group of the panel (noise canceling, transparency,
// normal ...). Default is the packed value written when the group is picked.
type ANCModeSpec struct {
	Name    string        `json:"name"`
	Start   int           `json:"start"`
	End     int           `json:"end"`
	Default int           `json:"default"`
	Params  []int         `json:"params"`
	Items   []ANCItemSpec `json:"items,omitempty"`
}

// EQSpec is the equalizer geometry a model declares. MinDb and MaxDb are the
// panel's own range as signed values (the server writes both as magnitudes).
// Slots are the effect ids of the panel (plus the personalised-listening id),
// ProtectedSlots the ones a write refuses to touch without the override.
type EQSpec struct {
	Bands          int       `json:"bands"`
	Freqs          []float64 `json:"freqs"`
	MinDb          float64   `json:"minDb"`
	MaxDb          float64   `json:"maxDb"`
	Slots          []int     `json:"slots"`
	ProtectedSlots []int     `json:"protectedSlots"`
}

// panelDerived is everything the profile of one model takes from its panel
// beyond the shared toggles.
type panelDerived struct {
	declared map[string]bool // shared action ids (anc, balance, ...)
	find     bool

	features []FeatureFlag // eq, keysRemap, fit, promptVolume
	actions  []ActionSpec  // promptVolume, wearSensitivity

	promptSteps []int
	wearItems   []NamedValue
	ancModes    []ANCModeSpec
	eq          *EQSpec
	notes       []string

	windOffZero, wearOffZero bool
}

// deriveFromPanel reads one product's panel. It never panics on partial data:
// a missing or malformed block just leaves the feature undeclared.
func deriveFromPanel(vendorID uint16, p models.Product) panelDerived {
	out := panelDerived{declared: map[string]bool{}}
	pn := p.Panel
	if pn == nil {
		return out
	}
	note := func(format string, a ...interface{}) { out.notes = append(out.notes, fmt.Sprintf(format, a...)) }

	// Shared toggles and selectors (existing mapping).
	if pn.Balance {
		out.declared["balance"] = true
	}
	if pn.AutoOff != nil {
		out.declared["autoOff"] = true
	}
	if pn.Wear != nil {
		out.declared["wearDetect"] = true
	}
	for _, o := range pn.Other {
		if o.Type == nil || o.CmdID == nil {
			continue
		}
		if *o.Type == 4 || *o.Type == 113 {
			if id, ok := toggleByCmd[*o.CmdID]; ok {
				out.declared[id] = true
			}
		}
	}
	out.find = pn.Find

	// ANC: only the packed (mode<<16)|(scene<<8)|level encoding is driven.
	switch {
	case pn.ANC != nil:
		if modes, ok := ancModesFromPanel(pn.ANC); ok {
			out.declared["anc"] = true
			out.ancModes = modes
		} else {
			note("anc: encoding not supported (the panel uses single-byte mode values)")
		}
	default:
		for _, f := range fnFunctionsFor(vendorID) {
			if f.Type != nil && (*f.Type == 6 || *f.Type == 11) {
				note("anc: encoding not supported (simple list / percent layout)")
				break
			}
		}
	}

	// EQ: the 10-band payload layout of 0x22.
	if pn.EQ != nil {
		switch {
		case pn.EQ.Bands == qcycoreEqBands && len(pn.EQ.Freq) == qcycoreEqBands:
			out.eq = eqSpecFromPanel(pn.EQ)
			out.features = append(out.features, FeatureEQ)
		default:
			note("eq: %d-band layout not supported (the payload builder writes %d bands)", pn.EQ.Bands, qcycoreEqBands)
		}
	}

	// Keys: the type-10 layout (event id + action id pairs).
	if ka := keyActionsFor(vendorID); len(ka) > 0 && hasType10Keys(vendorID) {
		out.features = append(out.features, FeatureKeysRemap)
	} else if pn.Keys != nil {
		note("keys: layout not supported (legacy per-gesture or button layout; only the type-10 music events are driven)")
	}

	// Fit test.
	if pn.Fit != nil && pn.Fit.CmdID != nil && *pn.Fit.CmdID == int(protocol.OpFit) {
		out.features = append(out.features, FeatureFit)
	}

	// Prompt volume: the panel's value list, written with the two-byte frame.
	if pn.Beep != nil {
		steps := promptStepsFromPanel(pn.Beep)
		switch {
		case pn.Beep.CmdID == nil || *pn.Beep.CmdID != int(protocol.OpPromptVolume):
			note("promptVolume: encoding not supported (command id differs from the shared one)")
		case len(steps) == 0:
			note("promptVolume: the panel lists no usable values")
		default:
			out.promptSteps = steps
			out.features = append(out.features, FeaturePromptVolume)
			out.actions = append(out.actions, ActionSpec{
				ID: "promptVolume", Label: "Prompt Volume", Type: "slider",
				Min: steps[0], Max: steps[len(steps)-1],
				Opcode: protocol.OpPromptVolume, ReadOpcode: protocol.OpPromptVolume,
			})
		}
	}

	// Wear-sensor sensitivity.
	if pn.Wear != nil && pn.Wear.Sensitivity != nil {
		s := pn.Wear.Sensitivity
		var items []NamedValue
		for _, it := range s.Items {
			if it.Cid != nil && *it.Cid >= 1 && *it.Cid <= 255 {
				items = append(items, NamedValue{Name: it.Name, Value: *it.Cid})
			}
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Value < items[j].Value })
		if s.CmdID != nil && *s.CmdID == int(protocol.OpWearSensitivity) && len(items) > 0 {
			out.wearItems = items
			out.actions = append(out.actions, ActionSpec{
				ID: "wearSensitivity", Label: "Wear Sensitivity", Type: "slider",
				Min: items[0].Value, Max: items[len(items)-1].Value, Step: 1,
				Opcode: protocol.OpWearSensitivity, ReadOpcode: protocol.OpWearSensitivity,
			})
		}
	}

	// Off bytes the panel itself declares.
	for _, f := range fnFunctionsFor(vendorID) {
		if f.CmdID == nil {
			continue
		}
		switch *f.CmdID {
		case int(protocol.OpWear):
			for _, c := range f.Choices() {
				if c.Value != nil && *c.Value == 0 {
					out.wearOffZero = true
				}
			}
		case int(protocol.OpWind):
			if t, ok := f.Toggle(); ok && len(t.Off) == 1 {
				if _, params, err := parsePanelFrame(t.Off[0], f.CmdID, false); err == nil && len(params) == 1 && params[0] == 0 {
					out.windOffZero = true
				}
			}
		}
	}
	return out
}

// qcycoreEqBands is the band count the 0x22 builder writes (qcycore.EqBands).
const qcycoreEqBands = 10

func hasType10Keys(vendorID uint16) bool {
	for _, f := range fnFunctionsFor(vendorID) {
		if f.Type != nil && *f.Type == 10 && f.Keys != nil {
			return true
		}
	}
	return false
}

// ancModesFromPanel converts the panel's ANC groups. ok=false when any value
// is not a packed one (below 0x10000) or does not fit mode/scene/level bytes.
func ancModesFromPanel(a *models.ANC) ([]ANCModeSpec, bool) {
	if a == nil || len(a.Modes) == 0 {
		return nil, false
	}
	valid := func(v int) bool { return v >= 1<<16 && v < 1<<24 }
	split := func(v int) []int { return []int{v >> 16, (v >> 8) & 0xff, v & 0xff} }
	var out []ANCModeSpec
	for _, m := range a.Modes {
		if m.Start == nil || !valid(*m.Start) {
			return nil, false
		}
		spec := ANCModeSpec{Name: m.Name, Start: *m.Start, End: *m.Start, Default: *m.Start}
		if m.End != nil {
			spec.End = *m.End
		}
		if m.Default != nil {
			spec.Default = *m.Default
		}
		if !valid(spec.End) || !valid(spec.Default) {
			return nil, false
		}
		spec.Params = split(spec.Default)
		for _, it := range m.Items {
			if it.Start == nil || !valid(*it.Start) {
				return nil, false
			}
			is := ANCItemSpec{Name: it.Name, Start: *it.Start, End: *it.Start}
			if it.End != nil {
				is.End = *it.End
			}
			if !valid(is.End) {
				return nil, false
			}
			is.Params = split(is.Start)
			spec.Items = append(spec.Items, is)
		}
		out = append(out, spec)
	}
	return out, true
}

// promptStepsFromPanel lists the beep values that fit the one-byte parameter,
// in the panel's order without duplicates.
func promptStepsFromPanel(b *models.Beep) []int {
	var out []int
	seen := map[int]bool{}
	for _, it := range b.Items {
		if it.Value < 1 || it.Value > 255 || seen[it.Value] {
			continue
		}
		seen[it.Value] = true
		out = append(out, it.Value)
	}
	sort.Ints(out)
	return out
}

// eqSpecFromPanel builds the EQ description. A sys_eq entry without an id gets
// its list position + 1, as the vendor parser assigns it. The protected slots
// are the personalised-listening slot the panel declares and the factory
// "classic" curve (id 6) when listed.
func eqSpecFromPanel(e *models.EQ) *EQSpec {
	abs := func(v float64) float64 {
		if v < 0 {
			return -v
		}
		return v
	}
	spec := &EQSpec{
		Bands: e.Bands, Freqs: append([]float64(nil), e.Freq...),
		MinDb: -abs(e.MinDb), MaxDb: abs(e.MaxDb),
		Slots: []int{}, ProtectedSlots: []int{},
	}
	seen := map[int]bool{}
	add := func(id int) {
		if id >= 0 && id <= 255 && !seen[id] {
			seen[id] = true
			spec.Slots = append(spec.Slots, id)
		}
	}
	for i, ef := range e.SysEq {
		id := i + 1
		if ef.ID != nil {
			id = *ef.ID
		}
		add(id)
	}
	protect := func(id int) {
		for _, p := range spec.ProtectedSlots {
			if p == id {
				return
			}
		}
		spec.ProtectedSlots = append(spec.ProtectedSlots, id)
	}
	if seen[6] {
		protect(6)
	}
	if e.Individuation != nil && e.Individuation.ID != nil {
		add(*e.Individuation.ID)
		protect(*e.Individuation.ID)
	}
	sort.Ints(spec.Slots)
	sort.Ints(spec.ProtectedSlots)
	return spec
}

var (
	n70Once   sync.Once
	n70Panel  panelDerived
	n70Vendor uint16 = 23877
)

// n70PanelParams is what the N70's own panel declares, for the parts the N70
// handler does not hard-code (ANC groups, EQ geometry).
func n70PanelParams() panelDerived {
	n70Once.Do(func() {
		if p, ok := models.Lookup(n70Vendor); ok && p.Panel != nil {
			n70Panel = deriveFromPanel(n70Vendor, p)
		}
	})
	return n70Panel
}
