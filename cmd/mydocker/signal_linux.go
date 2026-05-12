//go:build linux

package main

import "syscall"

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func sendTerm(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
func sendKill(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
