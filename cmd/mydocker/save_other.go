//go:build !linux

package main

// On non-Linux (where save will never actually run), define a zero struct
// so the Go compiler is happy. info.Sys().(*syscallStatT) will fail the
// type assertion and the hardlink branch is simply skipped.
type syscallStatT struct {
	Ino   uint64
	Nlink uint64
}
