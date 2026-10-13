package protocol

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

//go:embed table.json
var tableJSON []byte

// Entry is one row of the protocol table.
type Entry struct {
	Kind           string `json:"kind"`
	Opcode         byte   `json:"-"`
	HasOpcode      bool   `json:"-"`
	OpcodeHex      string `json:"opcodeHex"`
	Name           string `json:"name"`
	Direction      string `json:"direction"` // write, query or notify
	Frame          string `json:"frame"`
	Characteristic string `json:"characteristic"`
	ResponseNotify string `json:"responseOrNotify"`
	Preconditions  string `json:"preconditions"`
	PostActions    string `json:"postActions"`
	Models         string `json:"models"`
	Status         string `json:"status"`
}

type rawEntry struct {
	Entry
	Opcode *int `json:"opcode"`
}

// Registry is an immutable, validated command table.
type Registry struct {
	entries []Entry
	byKey   map[opKey]int
	byOp    map[byte][]int
	byName  map[string]int
}

type opKey struct {
	op  byte
	dir string
}

var validDirections = map[string]bool{"write": true, "query": true, "notify": true}

// Parse decodes and validates a table. It fails on malformed JSON, an opcode
// outside 0..255, an unknown direction, an empty or duplicate name, or two rows
// with the same (opcode, direction).
func Parse(data []byte) (*Registry, error) {
	var raws []rawEntry
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, fmt.Errorf("protocol table: %w", err)
	}
	if len(raws) == 0 {
		return nil, fmt.Errorf("protocol table: empty")
	}
	r := &Registry{
		byKey:  map[opKey]int{},
		byOp:   map[byte][]int{},
		byName: map[string]int{},
	}
	for i, re := range raws {
		e := re.Entry
		if e.Name == "" {
			return nil, fmt.Errorf("protocol table: row %d has no name", i)
		}
		if !validDirections[e.Direction] {
			return nil, fmt.Errorf("protocol table: %q: bad direction %q", e.Name, e.Direction)
		}
		if re.Opcode != nil {
			v := *re.Opcode
			if v < 0 || v > 0xFF {
				return nil, fmt.Errorf("protocol table: %q: opcode %d out of range", e.Name, v)
			}
			e.Opcode, e.HasOpcode = byte(v), true
			if e.OpcodeHex != "" && !strings.EqualFold(e.OpcodeHex, fmt.Sprintf("0x%02X", v)) {
				return nil, fmt.Errorf("protocol table: %q: opcodeHex %s does not match %d", e.Name, e.OpcodeHex, v)
			}
			k := opKey{e.Opcode, e.Direction}
			if j, dup := r.byKey[k]; dup {
				return nil, fmt.Errorf("protocol table: %q and %q share opcode 0x%02X direction %s", r.entries[j].Name, e.Name, v, e.Direction)
			}
			r.byKey[k] = i
			r.byOp[e.Opcode] = append(r.byOp[e.Opcode], i)
		}
		if _, dup := r.byName[e.Name]; dup {
			return nil, fmt.Errorf("protocol table: duplicate name %q", e.Name)
		}
		r.byName[e.Name] = i
		r.entries = append(r.entries, e)
	}
	return r, nil
}

// Lookup returns the entry for opcode. When the opcode has several rows (for
// example a write and a query form) the write row wins.
func (r *Registry) Lookup(opcode byte) (Entry, bool) {
	idx := r.byOp[opcode]
	if len(idx) == 0 {
		return Entry{}, false
	}
	for _, i := range idx {
		if r.entries[i].Direction == "write" {
			return r.entries[i], true
		}
	}
	return r.entries[idx[0]], true
}

// LookupDir returns the entry for an exact (opcode, direction) pair.
func (r *Registry) LookupDir(opcode byte, direction string) (Entry, bool) {
	i, ok := r.byKey[opKey{opcode, direction}]
	if !ok {
		return Entry{}, false
	}
	return r.entries[i], true
}

// ByName returns the entry with exactly this name.
func (r *Registry) ByName(name string) (Entry, bool) {
	i, ok := r.byName[name]
	if !ok {
		return Entry{}, false
	}
	return r.entries[i], true
}

// All returns a copy of every row, in table order.
func (r *Registry) All() []Entry { return append([]Entry(nil), r.entries...) }

var (
	defOnce sync.Once
	defReg  *Registry
	defErr  error
)

// Default is the embedded table. A malformed embedded table is a build defect:
// it panics rather than letting a caller run without a registry.
func Default() *Registry {
	defOnce.Do(func() { defReg, defErr = Parse(tableJSON) })
	if defErr != nil {
		panic(defErr)
	}
	return defReg
}

// Lookup is Default().Lookup.
func Lookup(opcode byte) (Entry, bool) { return Default().Lookup(opcode) }

// ByName is Default().ByName.
func ByName(name string) (Entry, bool) { return Default().ByName(name) }

// All is Default().All.
func All() []Entry { return Default().All() }
