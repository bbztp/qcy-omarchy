package qcycore

import "fmt"

// FactoryEffect is one of the earbuds' factory EQ effects as the phone app
// lists them: the slot the device reports it under, its display name and the
// 0x22 payload read back from a test device (QCY MeloBuds N70) with that effect active.
type FactoryEffect struct {
	Slot    int
	Name    string
	Payload string // 143 bytes, lowercase hex, exactly as the device reported it
}

// HearingTestName and HearingTestSlot identify the user's own hearing-test
// profile ("Experiência auditiva"): it is per user, so its payload is never
// embedded here; the library seed takes it from the backups.
const (
	HearingTestName = "Experiência auditiva"
	HearingTestSlot = 11
)

// FactoryEffects are the factory curves, in the order the phone app shows them
// (Agudos suaves comes before Som espacial). They are public factory data,
// measured on the device (reads of 0x22 with each effect active; the 0xFF type
// bytes mark empty positions). Som espacial (slot 10) is slot 1 with byte 0
// changed. The library seed stores one curve per entry (source
// factory-snapshot, id Slug(Name)).
var FactoryEffects = []FactoryEffect{
	{Slot: 1, Name: "Predefinido",
		Payload: "0150fbc8007cfc46000284039cff460002f00ac800780002342144fd5e0102581b9cff780003983a38ff900102000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff"},
	{Slot: 2, Name: "Popular",
		Payload: "0250fb3c00d4fe320002c8007cfc46000284039cff460002f00ac800780002401f44fd5e0102000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff"},
	{Slot: 3, Name: "Bass pesados",
		Payload: "0350fb3c00f401320002c80044fd46000284039cff460002f00a6400780002401f44fd500002000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff"},
	{Slot: 4, Name: "Rock",
		Payload: "0450fb3c00f401320002c80050fb460002840338ff460002f00ac800780002401f44fd5e0102000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff"},
	{Slot: 5, Name: "Suave",
		Payload: "0550fb320038ff320002c8007cfc46000284039cff460002f00a6400780002401fe0fc640002000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff"},
	{Slot: 6, Name: "Clássico",
		Payload: "0650fb3c002c01320002c8000cfe46000284039cff460002f00ac800780002401f44fd5e0102000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff"},
	{Slot: 14, Name: "Agudos suaves",
		Payload: "0e50fb3200c800320002c800aefc46000284039cff460002f00ac8007800023421e6fb7c0102581b9cff780003983a06ff900102000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff"},
	{Slot: 10, Name: "Som espacial",
		Payload: "0a50fbc8007cfc46000284039cff460002f00ac800780002342144fd5e0102581b9cff780003983a38ff900102000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff000000000000ff"},
}

// FactoryCurveID is the library id the seed gives to a factory effect.
func FactoryCurveID(e FactoryEffect) string { return Slug(e.Name) }

// HearingTestCurveID is the library id of the hearing-test curve.
func HearingTestCurveID() string { return Slug(HearingTestName) }

// SupersededCurveIDs maps the ids older seeds produced ("Clássico (slot 6)",
// "Slot 11 (primeira leitura)", "Predefinido (slot 1)", ...) to the id of the
// curve that replaces them. `library list` hides a key while its value is in
// the library. Slot 0 has no entry: nothing replaces the old test curve.
func SupersededCurveIDs() map[string]string {
	m := map[string]string{}
	add := func(slot int, name, id string) {
		m[Slug(fmt.Sprintf("%s (slot %d)", name, slot))] = id
		m[Slug(fmt.Sprintf("Slot %d (primeira leitura)", slot))] = id
	}
	for _, e := range FactoryEffects {
		add(e.Slot, e.Name, FactoryCurveID(e))
	}
	add(HearingTestSlot, HearingTestName, HearingTestCurveID())
	return m
}
