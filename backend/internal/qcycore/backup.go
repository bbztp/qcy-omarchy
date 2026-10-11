package qcycore

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BackupDir returns the directory EQ backups live in. It is under the data
// directory, never the cache: a cache cleaner must not be able to eat the only
// copy of a hearing-test curve.
func BackupDir() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" || !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("backup dir: %w", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "qcy-omarchy", "backups"), nil
}

// BackupFiles are the paths one backup call produced.
type BackupFiles struct {
	First   string // eq-slot<N>-first.hex (immutable; present whether or not created now)
	History string // eq-slot<N>-<UTC>.hex (append-only)
}

// SaveEqBackup writes payload (the raw 0x22 bytes read from the device, BEFORE
// a write) to the default backup directory. Callers must abort the device write
// when it returns an error.
func SaveEqBackup(payload []byte) (BackupFiles, error) {
	dir, err := BackupDir()
	if err != nil {
		return BackupFiles{}, err
	}
	if err := EnsureBackupDir(dir); err != nil {
		return BackupFiles{}, err
	}
	importLegacyBackups(dir)
	return SaveEqBackupTo(dir, payload, time.Now())
}

// EnsureBackupDir creates dir (0700) and tightens it if it already exists.
func EnsureBackupDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("backup dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("backup dir: %w", err)
	}
	return nil
}

// SaveEqBackupTo stores payload in dir as hex, twice:
//
//   - eq-slot<N>-first.hex: written only if absent, never replaced, so the very
//     first payload ever seen for a slot (the vendor curve) survives forever;
//   - eq-slot<N>-<UTC timestamp>.hex: one new file per call, never replaced.
//
// Each file is written to a temp file, fsynced and then hard-linked into place,
// which fails if the name exists (unlike rename, which would overwrite).
func SaveEqBackupTo(dir string, payload []byte, now time.Time) (BackupFiles, error) {
	if err := ValidateEq(payload); err != nil {
		return BackupFiles{}, err
	}
	if err := EnsureBackupDir(dir); err != nil {
		return BackupFiles{}, err
	}
	slot := int(payload[0])
	line := []byte(hex.EncodeToString(payload) + "\n")
	files := BackupFiles{First: filepath.Join(dir, fmt.Sprintf("eq-slot%d-first.hex", slot))}

	if err := writeNoReplace(files.First, line); err != nil && !errors.Is(err, os.ErrExist) {
		return BackupFiles{}, fmt.Errorf("backup first: %w", err)
	}
	ts := now.UTC().Format("20060102T150405.000000Z")
	for n := 0; n < 100; n++ {
		name := fmt.Sprintf("eq-slot%d-%s.hex", slot, ts)
		if n > 0 {
			name = fmt.Sprintf("eq-slot%d-%s-%d.hex", slot, ts, n)
		}
		p := filepath.Join(dir, name)
		err := writeNoReplace(p, line)
		if err == nil {
			files.History = p
			return files, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return BackupFiles{}, fmt.Errorf("backup history: %w", err)
		}
	}
	return BackupFiles{}, errors.New("backup history: could not find a free file name")
}

// writeNoReplace writes data to a temp file in the same directory, fsyncs it
// and links it to path. It returns an error wrapping os.ErrExist when path is
// already there, leaving the existing file untouched.
func writeNoReplace(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-backup-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpName, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// importLegacyBackups copies the JSON backups older builds left in the cache
// directory into dir (as legacy-<name>), without removing or overwriting
// anything. A marker file records that it completed, so it runs once.
func importLegacyBackups(dir string) {
	marker := filepath.Join(dir, ".legacy-imported")
	if _, err := os.Stat(marker); err == nil {
		return
	}
	var srcs []string
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		srcs = append(srcs, filepath.Join(x, "qcyctl"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		srcs = append(srcs, filepath.Join(home, ".cache", "qcyctl"))
	}
	ok := true
	seen := map[string]bool{}
	for _, src := range srcs {
		if seen[src] {
			continue
		}
		seen[src] = true
		entries, err := os.ReadDir(src)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasPrefix(name, "eq-backup") || !strings.HasSuffix(name, ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(src, name))
			if err != nil {
				ok = false
				continue
			}
			if err := writeNoReplace(filepath.Join(dir, "legacy-"+name), data); err != nil && !errors.Is(err, os.ErrExist) {
				ok = false
			}
		}
	}
	if ok {
		_ = writeNoReplace(marker, []byte("legacy cache backups copied; originals left in place\n"))
	}
}
