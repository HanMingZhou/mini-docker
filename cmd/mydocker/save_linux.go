//go:build linux

package main

import "syscall"

// syscallStatT is the platform-specific stat struct used for hardlink
// detection in save.go's tarDirIntoArchive.
type syscallStatT = syscall.Stat_t
