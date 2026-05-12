//go:build !linux

package store

// hasFileLock 表明本平台 WithLock 是 no-op。
// 用于单元测试的条件编译。
const hasFileLock = false

// WithLock 在非 Linux 平台是 no-op（只是执行 fn）。
// 非 Linux 平台没有 flock，本项目也不在非 Linux 上实际运行容器，
// 只是为了让代码能在 macOS / Windows 上编译通过、跑单元测试。
func (s *Store) WithLock(fn func() error) error {
	return fn()
}
