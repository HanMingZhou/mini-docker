//go:build linux

package main

import (
	"errors"
	"syscall"
)

// pidAlive 检查指定 PID 的进程是否存在。
//
// 用 kill(pid, 0) 探活：
//   - 进程存在且有权限发信号 → 返回 nil，存活
//   - 进程不存在 → ESRCH，已死
//   - 进程存在但无权限（如 root 跑的容器，普通用户去 ps）→ EPERM
//
// EPERM 仍然意味着进程存在，必须当作"存活"。否则普通用户跑
// `mydocker ps` 时会把 root 创建的容器全部误标成 Exited，
// 进而被 listContainers 写回 store 破坏状态。
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

func sendTerm(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
func sendKill(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
