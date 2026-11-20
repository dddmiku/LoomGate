//go:build !windows

package hotupdate

import (
	"os"
	"syscall"
)

func lockRepair(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
