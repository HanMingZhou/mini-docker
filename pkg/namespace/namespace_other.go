//go:build !linux

package namespace

// CloneFlags 在非 Linux 平台返回 0，仅为了让 IDE / 语法检查能通过。
// 产物只能在 Linux 上运行。
func (f Flags) CloneFlags() uintptr {
	return 0
}
