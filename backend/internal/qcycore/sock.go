package qcycore

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// SocketPath is where qcyctld listens. $XDG_RUNTIME_DIR is already private to
// the user. Without it the socket goes in /tmp/qcyctl-<uid>, a directory that
// must be a real directory owned by this user with mode 0700; anything else
// (pre-created by another user, a symlink, group/world accessible) is refused
// so a local attacker cannot stand in for the daemon or its clients.
//
// create=true (daemon) makes the fallback directory; clients only validate.
func SocketPath(create bool) (string, error) {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "qcyctld.sock"), nil
	}
	dir := filepath.Join("/tmp", fmt.Sprintf("qcyctl-%d", os.Getuid()))
	if create {
		if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
			return "", fmt.Errorf("socket dir: %w", err)
		}
	}
	if err := checkPrivateDir(dir); err != nil {
		return "", err
	}
	return filepath.Join(dir, "qcyctld.sock"), nil
}

func checkPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("socket dir: %w", err)
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("socket dir %s is not a plain directory", dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("socket dir %s is not owned by the current user", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("socket dir %s has mode %o, want 0700", dir, fi.Mode().Perm())
	}
	return nil
}
