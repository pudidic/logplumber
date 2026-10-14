//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on the file. The OS releases it even if the process crashes.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, new(windows.Overlapped)); err != nil {
		f.Close()
		return nil, errLocked
	}
	return f, nil
}
