//go:build linux

package namespace

import "syscall"

// CloneFlags 把 Flags 转换成 Linux clone(2) 标志位。
func (f Flags) CloneFlags() uintptr {
	var cf uintptr
	if f.UTS {
		cf |= syscall.CLONE_NEWUTS
	}
	if f.PID {
		cf |= syscall.CLONE_NEWPID
	}
	if f.Mount {
		cf |= syscall.CLONE_NEWNS
	}
	if f.Network {
		cf |= syscall.CLONE_NEWNET
	}
	if f.IPC {
		cf |= syscall.CLONE_NEWIPC
	}
	if f.User {
		cf |= syscall.CLONE_NEWUSER
	}
	return cf
}
