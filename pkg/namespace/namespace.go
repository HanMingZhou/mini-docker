// Package namespace 封装创建隔离 namespace 的系统调用。
//
// Level 1 仅支持 UTS / PID / Mount / Network / IPC 五种 namespace。
// User / Cgroup namespace 放到后续 Level。
package namespace

// Flags 描述一组 namespace 的开关。
type Flags struct {
	UTS     bool // 主机名隔离
	PID     bool // 进程号隔离
	Mount   bool // 挂载点隔离
	Network bool // 网络隔离
	IPC     bool // IPC 隔离
	User    bool // uid/gid 隔离（进阶）
}

// Default 返回 Level 1 推荐的一组 namespace 开关。
func Default() Flags {
	return Flags{
		UTS:     true,
		PID:     true,
		Mount:   true,
		Network: true,
		IPC:     true,
		User:    false,
	}
}
