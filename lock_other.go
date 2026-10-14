//go:build !(linux || darwin || freebsd || openbsd || netbsd || windows)

package main

import "os"

// On platforms without file locking support, open the file without locking it.
func lockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
}
