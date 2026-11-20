//go:build windows

package hotupdate

import (
	"os"
	"syscall"
	"unsafe"
)

var repairLockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

func lockRepair(file *os.File) error {
	var overlapped syscall.Overlapped
	result, _, err := repairLockFileEx.Call(file.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if result == 0 {
		return err
	}
	return nil
}
