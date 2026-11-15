//go:build windows

package accountvault

import (
	"errors"
	"os"
	"os/user"
	"syscall"
	"unsafe"
)

var advapi = syscall.NewLazyDLL("advapi32.dll")
var convertSD = advapi.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
var setSecurity = advapi.NewProc("SetFileSecurityW")
var kernel = syscall.NewLazyDLL("kernel32.dll")
var localFree = kernel.NewProc("LocalFree")
var lockEx = kernel.NewProc("LockFileEx")

func secureDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("vault_directory_failed")
	}
	u, err := user.Current()
	if err != nil {
		return errors.New("vault_identity_failed")
	}
	descriptor, err := syscall.UTF16PtrFromString("D:P(A;OICI;FA;;;" + u.Uid + ")(A;OICI;FA;;;SY)")
	if err != nil {
		return errors.New("vault_identity_failed")
	}
	var sd uintptr
	ok, _, _ := convertSD.Call(uintptr(unsafe.Pointer(descriptor)), 1, uintptr(unsafe.Pointer(&sd)), 0)
	if ok == 0 {
		return errors.New("vault_permissions_failed")
	}
	defer localFree.Call(sd)
	path, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return errors.New("vault_path_invalid")
	}
	ok, _, _ = setSecurity.Call(uintptr(unsafe.Pointer(path)), 0x80000004, sd)
	if ok == 0 {
		return errors.New("vault_permissions_failed")
	}
	return nil
}
func isLink(path string) bool {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return true
	}
	attributes, err := syscall.GetFileAttributes(p)
	return err == nil && attributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
func lockOwner(f *os.File) error {
	var o syscall.Overlapped
	r, _, err := lockEx.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&o)))
	if r == 0 {
		return err
	}
	return nil
}
