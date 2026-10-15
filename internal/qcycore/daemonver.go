package qcycore

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// VersionFilePath is where the running daemon records its pid and version, next
// to its socket. Asking the daemon over the socket would open a BLE session on
// daemons that predate the request, so the version is published as a file.
func VersionFilePath(create bool) (string, error) {
	sock, err := SocketPath(create)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(sock), "qcyctld.version"), nil
}

// WriteDaemonVersion records "<pid> <version>" for the daemon process pid.
func WriteDaemonVersion(pid int, version string) error {
	p, err := VersionFilePath(true)
	if err != nil {
		return err
	}
	return WriteFileAtomic(p, []byte(fmt.Sprintf("%d %s\n", pid, version)), 0o600)
}

// ReadDaemonVersion returns the version recorded by process pid, or "" when the
// file is absent, unreadable or written by another process (a stale file left
// by a crash, or a daemon that predates it).
func ReadDaemonVersion(pid int) string {
	p, err := VersionFilePath(false)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	f := strings.Fields(string(b))
	if len(f) != 2 || f[0] != strconv.Itoa(pid) {
		return ""
	}
	return f[1]
}

// RemoveDaemonVersion deletes the record when it still belongs to pid.
func RemoveDaemonVersion(pid int) {
	if ReadDaemonVersion(pid) == "" {
		return
	}
	if p, err := VersionFilePath(false); err == nil {
		_ = os.Remove(p)
	}
}
