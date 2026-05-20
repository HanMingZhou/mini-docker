//go:build linux

package cri

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// pidAlive returns true if a process with the given pid exists.
//
// Uses kill(pid, 0):
//   - nil  → process exists and we have permission
//   - EPERM → process exists but we lack permission (still alive!)
//   - ESRCH → process does not exist
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

// bindMountIntoRootfs bind-mounts hostPath into mergedRoot/containerPath.
func bindMountIntoRootfs(mergedRoot, hostPath, containerPath string, readonly bool) error {
	target := filepath.Join(mergedRoot, containerPath)
	// Create mount target (file or dir depending on source)
	if info, err := os.Stat(hostPath); err == nil && !info.IsDir() {
		_ = os.MkdirAll(filepath.Dir(target), 0755)
		f, _ := os.OpenFile(target, os.O_CREATE, 0644)
		if f != nil {
			_ = f.Close()
		}
	} else {
		_ = os.MkdirAll(target, 0755)
	}
	flags := uintptr(syscall.MS_BIND | syscall.MS_REC)
	if err := syscall.Mount(hostPath, target, "", flags, ""); err != nil {
		return err
	}
	if readonly {
		_ = syscall.Mount("", target, "", flags|syscall.MS_RDONLY|syscall.MS_REMOUNT, "")
	}
	return nil
}
