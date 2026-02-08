//go:build windows

package state

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx = modkernel32.NewProc("UnlockFileEx")
)

const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
)

func lockFileExclusive(f *os.File) error {
	h := syscall.Handle(f.Fd())
	ol := new(syscall.Overlapped)
	r1, _, err := procLockFileEx.Call(
		uintptr(h),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0,
		1, 0,
		uintptr(unsafe.Pointer(ol)),
	)
	if r1 == 0 {
		return err
	}
	return nil
}

func unlockFile(f *os.File) error {
	h := syscall.Handle(f.Fd())
	ol := new(syscall.Overlapped)
	r1, _, err := procUnlockFileEx.Call(
		uintptr(h),
		0,
		1, 0,
		uintptr(unsafe.Pointer(ol)),
	)
	if r1 == 0 {
		return err
	}
	return nil
}
