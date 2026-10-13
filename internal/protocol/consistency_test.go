package protocol

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// opcodeConsts reads the Op* constants from opcodes.go, so this test is the
// check that the source tree and the registry agree without a second list.
func opcodeConsts(t *testing.T) map[string]byte {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "opcodes.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]byte{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, sp := range gd.Specs {
			vs := sp.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if !strings.HasPrefix(n.Name, "Op") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					continue
				}
				v, err := strconv.ParseUint(lit.Value, 0, 8)
				if err != nil {
					t.Fatalf("%s: %v", n.Name, err)
				}
				out[n.Name] = byte(v)
			}
		}
	}
	return out
}

var callSite = regexp.MustCompile(`\.(send|setConfirm|query|queryLocked|awaitMatch|wait)\(\s*(protocol\.Op\w+|0x[0-9A-Fa-f]{2})\b`)

// Every opcode the daemon and the CLI pass to a send or query call is a named
// registry constant (no bare literals), exists in the table, and a written
// opcode has a write row (or is the 0xFE query envelope).
func TestSendersUseRegisteredOpcodes(t *testing.T) {
	consts := opcodeConsts(t)
	seen := 0
	for _, root := range []string{"../../cmd", "../../internal"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range callSite.FindAllStringSubmatch(string(src), -1) {
				seen++
				verb, arg := m[1], m[2]
				if !strings.HasPrefix(arg, "protocol.") {
					t.Errorf("%s: %s(%s): bare opcode literal, use a protocol constant", path, verb, arg)
					continue
				}
				op, ok := consts[strings.TrimPrefix(arg, "protocol.")]
				if !ok {
					t.Errorf("%s: unknown constant %s", path, arg)
					continue
				}
				e, ok := Lookup(op)
				if !ok {
					t.Errorf("%s: %s(%s): 0x%02x not in the registry", path, verb, arg, op)
					continue
				}
				if (verb == "send" || verb == "setConfirm") && op != OpQuery && e.Direction != "write" {
					t.Errorf("%s: %s(%s): sent as a write but the registry row is %q", path, verb, arg, e.Direction)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if seen < 10 {
		t.Fatalf("scan found only %d call sites; the pattern is stale", seen)
	}
}

// The action names of the write path map to the registry rows they drive; the
// name fragment is what the table calls the command.
func TestActionNamesMatchRegistry(t *testing.T) {
	frag := map[string]string{
		"anc": "ANC", "game": "Game", "sleep": "Sleep", "ldac": "LDAC",
		"multipoint": "Multipoint", "wind": "Wind", "promptVol": "Prompt",
		"autoOff": "power-off", "wear": "Wear", "balance": "balance",
		"spatial": "Spatial", "fit": "Fit", "wearSensitivity": "sensitivity",
		"find": "LED",
	}
	if len(frag) != len(ActionOpcodes) {
		t.Fatalf("%d fragments for %d actions", len(frag), len(ActionOpcodes))
	}
	for action, op := range ActionOpcodes {
		e, ok := Lookup(op)
		if !ok {
			t.Errorf("%s: 0x%02x missing", action, op)
			continue
		}
		if !strings.Contains(strings.ToLower(e.Name), strings.ToLower(frag[action])) {
			t.Errorf("action %s -> 0x%02x is named %q in the registry (want %q in it)", action, op, e.Name, frag[action])
		}
	}
}
