//go:build !windows

package accountvault

import (
	"errors"
	"os"
	"syscall"
)

func secureDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("vault_directory_failed")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return errors.New("vault_permissions_failed")
	}
	return nil
}
func isLink(path string) bool {
	f, err := os.Lstat(path)
	return err == nil && f.Mode()&os.ModeSymlink != 0
}
func lockOwner(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) }
