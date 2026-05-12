//go:build linux

package main

import "syscall"

// Signals only available on Linux, merged into sigNameMap at init time.
func init() {
	sigNameMap["STKFLT"] = syscall.SIGSTKFLT
	sigNameMap["PWR"] = syscall.SIGPWR
}
