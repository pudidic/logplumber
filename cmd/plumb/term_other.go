//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd

package main

import "os"

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func enableVT(*os.File) bool       { return os.Getenv("TERM") != "dumb" }
func termSize(*os.File) (int, int) { return 0, 0 }
func initConsole()                 {}
