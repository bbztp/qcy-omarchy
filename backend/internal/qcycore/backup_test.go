package qcycore

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupFirstIsImmutableAndHistoryAppends(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backups")
	t0 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

	a := fixture(11, 0)
	b := fixture(11, 300)

	fa, err := SaveEqBackupTo(dir, a, t0)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := SaveEqBackupTo(dir, b, t0) // same instant: must not collide
	if err != nil {
		t.Fatal(err)
	}
	if fa.First != fb.First || filepath.Base(fa.First) != "eq-slot11-first.hex" {
		t.Fatalf("first = %q / %q", fa.First, fb.First)
	}
	if fa.History == fb.History {
		t.Fatal("history entries collided")
	}
	if !strings.Contains(filepath.Base(fa.History), "20261011T120000.000000Z") {
		t.Fatalf("history name %q", fa.History)
	}

	// first keeps payload a; history entries keep their own payloads
	assertHex(t, fa.First, a)
	assertHex(t, fa.History, a)
	assertHex(t, fb.History, b)

	// permissions
	for _, p := range []string{fa.First, fa.History, fb.History} {
		fi, _ := os.Stat(p)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %o", p, fi.Mode().Perm())
		}
	}
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %o", di.Mode().Perm())
	}
	// no temp files left behind
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestBackupPerSlotAndRejectsBadPayload(t *testing.T) {
	dir := t.TempDir()
	f0, err := SaveEqBackupTo(dir, fixture(0, 0), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f11, err := SaveEqBackupTo(dir, fixture(11, 0), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(f0.First) != "eq-slot0-first.hex" || filepath.Base(f11.First) != "eq-slot11-first.hex" {
		t.Fatalf("%s %s", f0.First, f11.First)
	}
	if _, err := SaveEqBackupTo(dir, []byte{1, 2, 3}, time.Now()); err == nil {
		t.Fatal("short payload backed up")
	}
}

func TestBackupFailureIsReported(t *testing.T) {
	// A path under a regular file cannot become a directory: the error must
	// surface so the caller can abort the device write.
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveEqBackupTo(filepath.Join(file, "sub"), fixture(1, 0), time.Now()); err == nil {
		t.Fatal("backup into an impossible directory reported success")
	}
}

func TestLegacyImportCopiesWithoutDeleting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	legacy := filepath.Join(home, ".cache", "qcyctl")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(legacy, "eq-backup.json"), []byte(`[{"preset":11}]`), 0o644)
	os.WriteFile(filepath.Join(legacy, "eq-backup-slot0.json"), []byte(`[{"preset":0}]`), 0o644)
	os.WriteFile(filepath.Join(legacy, "le-address"), []byte("AA"), 0o644)

	files, err := SaveEqBackup(fixture(11, 0))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".local", "share", "qcy-omarchy", "backups")
	if filepath.Dir(files.First) != want {
		t.Fatalf("dir = %s want %s", filepath.Dir(files.First), want)
	}
	for _, n := range []string{"legacy-eq-backup.json", "legacy-eq-backup-slot0.json"} {
		b, err := os.ReadFile(filepath.Join(want, n))
		if err != nil || len(b) == 0 {
			t.Errorf("%s not imported: %v", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(want, "legacy-le-address")); err == nil {
		t.Error("non-backup file imported")
	}
	if _, err := os.Stat(filepath.Join(legacy, "eq-backup.json")); err != nil {
		t.Error("legacy original was removed")
	}
	if _, err := os.Stat(filepath.Join(home, ".cache", "qcyctl", "eq-slot11-first.hex")); err == nil {
		t.Error("new backup written into the cache")
	}
}

func assertHex(t *testing.T, path string, want []byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || string(got) != string(want) {
		t.Fatalf("%s = %q, want %x", path, b, want)
	}
}
