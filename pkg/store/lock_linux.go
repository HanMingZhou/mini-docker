//go:build linux

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// hasFileLock 表明本平台 WithLock 是真实的 flock（非 no-op）。
// 用于单元测试的条件编译。
const hasFileLock = true

// WithLock 在 <root>/.lock 上持有一把 flock 排他锁，执行 fn，然后释放。
// 用于跨进程互斥（两个 mydocker CLI 进程同时 run --name web 之类）。
//
// 典型用法：
//
//	st.WithLock(func() error {
//	    if _, err := st.Resolve(name); err == nil {
//	        return fmt.Errorf("name in use")
//	    }
//	    return st.Save(newContainer)
//	})
//
// flock 的语义：
//   - 文件级锁，只要 fd 持有者唯一，就互斥
//   - fd 关闭时自动释放（进程崩溃也不会死锁）
//   - 同一进程的多个 goroutine 共享同一 fd 时不互斥，所以配合 sync.Mutex 使用
func (s *Store) WithLock(fn func() error) error {
	lockPath := filepath.Join(s.root, ".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		return fmt.Errorf("mkdir lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	return fn()
}
