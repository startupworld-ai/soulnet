//go:build windows

package main

import (
	"os"
	"syscall"
)

// ERROR_SHARING_VIOLATION: the file is open in a share mode that denies us.
const errorSharingViolation syscall.Errno = 32

// openLocked opens the lock file for writing while sharing only READ: any
// second writer gets a sharing violation, so exactly one peer can hold it,
// while others may still read the holder pid out of it. Windows drops the
// share restriction when the holding handle closes — i.e. when the holder
// exits, however it exits — which is exactly the stale-lock takeover we want.
func openLocked(path string) (*os.File, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := syscall.CreateFile(
		pathPtr,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ, // readers welcome, second writer = sharing violation
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if errno, ok := err.(syscall.Errno); ok && errno == errorSharingViolation {
			return nil, errLockHeld
		}
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}
