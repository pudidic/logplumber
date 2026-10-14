//go:build linux || darwin || freebsd || openbsd || netbsd

package main

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive lock on the file. The OS releases it even if the process crashes.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errLocked
	}
	return f, nil
}
