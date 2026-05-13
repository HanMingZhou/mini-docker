//go:build linux

package rootfs

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func setup(newRoot string, extraMounts []Mount) error {
	// 0. 把整棵树改成 private，否则 pivot_root 里的 mount 会泄漏到宿主机
	if err := syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}

	// 1. bind mount newRoot 到自身，满足 pivot_root 的要求
	if err := syscall.Mount(newRoot, newRoot, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind mount newRoot: %w", err)
	}

	// 2. 准备 .pivot_root 目录
	pivotDir := filepath.Join(newRoot, ".pivot_root")
	if err := os.MkdirAll(pivotDir, 0700); err != nil {
		return fmt.Errorf("mkdir pivot_root: %w", err)
	}

	// 3. pivot_root：新 root -> /, 老 root -> /.pivot_root
	if err := syscall.PivotRoot(newRoot, pivotDir); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}

	// 4. 切到新根
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}

	// 5. umount 老 root（此时它挂在 /.pivot_root）
	const oldRoot = "/.pivot_root"
	if err := syscall.Unmount(oldRoot, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	if err := os.Remove(oldRoot); err != nil {
		return fmt.Errorf("remove old root dir: %w", err)
	}

	// 6. 挂必要的伪文件系统
	if err := mountDefaults(); err != nil {
		return err
	}

	// 7. 处理用户 mount（此时 source 路径还是宿主机视角，因为 bind mount 的
	//    第一个参数总是相对当前 mount namespace。pivot_root 之后，挂在宿主机上
	//    的文件系统已经不在当前 namespace 视野里，所以这一步要求调用方在**父进程**
	//    中就把 Source 解析成绝对路径并确认可访问——volume 的目录**必须在宿主机上
	//    真实存在**。这里只需按绝对路径 bind 即可。）
	//
	// 注意：父子进程共享 mount namespace 时 pivot_root 的挂载会被带回宿主机，
	// 所以在父进程里 prepare 的 bind mount 要么 rootfs 组装前就挂好（在 merged 下），
	// 要么等到子进程里再挂。这里我们用**子进程里再挂**的方式，source 依赖……
	//
	// 实际经验：pivot_root 之后，原有的挂载点是否可见取决于 make-rprivate 的效果；
	// 对 overlay+bind 的简单场景，如果在**调用 rootfs.Setup 之前**（即子进程入口）
	// source 路径还能访问，那在 pivot_root 完成后就访问不到了。为了简化，我们
	// 改成"在父进程里 bind mount 到 merged/<target>"，这里 extraMounts 就无事可做。
	// 为保持接口对称，这里仍然接收参数但直接返回 nil。
	_ = extraMounts
	return nil
}

func mountDefaults() error {
	type m struct {
		source, target, fstype string
		flags                  uintptr
		data                   string
	}
	mounts := []m{
		{"proc", "/proc", "proc", syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV, ""},
		{"sysfs", "/sys", "sysfs", syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV | syscall.MS_RDONLY, ""},
		{"tmpfs", "/dev", "tmpfs", syscall.MS_NOSUID | syscall.MS_STRICTATIME, "mode=755"},
	}
	for _, mt := range mounts {
		if err := os.MkdirAll(mt.target, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", mt.target, err)
		}
		if err := syscall.Mount(mt.source, mt.target, mt.fstype, mt.flags, mt.data); err != nil {
			return fmt.Errorf("mount %s on %s: %w", mt.source, mt.target, err)
		}
	}
	return nil
}

// ApplyBindMounts 在**父进程**（子进程启动前）把 volume bind mount 到 merged 根。
// 这样等子进程 pivot_root 后，挂载点就在新根里可见。
// source 必须是宿主机上的绝对路径且目录已存在；target 是 merged 内的相对或绝对路径。
func ApplyBindMounts(merged string, mounts []Mount) error {
	for _, m := range mounts {
		if m.Source == "" || m.Target == "" {
			return fmt.Errorf("empty mount source/target: %+v", m)
		}
		if !filepath.IsAbs(m.Source) {
			return fmt.Errorf("mount source must be absolute: %q", m.Source)
		}
		rel := m.Target
		if filepath.IsAbs(rel) {
			rel = rel[1:] // 去掉前导 /
		}
		target := filepath.Join(merged, rel)

		// 根据 source 是文件还是目录来准备 target
		src, err := os.Stat(m.Source)
		if err != nil {
			return fmt.Errorf("stat source %s: %w", m.Source, err)
		}
		if src.IsDir() {
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir %s: %w", filepath.Dir(target), err)
			}
			// 单文件 bind mount 需要 target 存在
			if _, err := os.Stat(target); os.IsNotExist(err) {
				f, err := os.Create(target)
				if err != nil {
					return fmt.Errorf("create %s: %w", target, err)
				}
				_ = f.Close()
			}
		}

		flags := uintptr(syscall.MS_BIND | syscall.MS_REC)
		if err := syscall.Mount(m.Source, target, "", flags, ""); err != nil {
			return fmt.Errorf("bind mount %s -> %s: %w", m.Source, target, err)
		}
		if m.ReadOnly {
			// bind 完 remount 才能生效 ro
			if err := syscall.Mount("", target, "",
				flags|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
				return fmt.Errorf("remount ro %s: %w", target, err)
			}
		}
	}
	return nil
}
