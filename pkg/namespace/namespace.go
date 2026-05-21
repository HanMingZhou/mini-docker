// Package namespace 封装创建隔离 namespace 的系统调用。
//
// 支持 UTS / PID / Mount / Network / IPC / Cgroup 六种 namespace。
// User namespace 留作进阶扩展。
package namespace

// Flags 描述一组 namespace 的开关。
type Flags struct {
	UTS     bool // 主机名隔离
	PID     bool // 进程号隔离
	Mount   bool // 挂载点隔离
	Network bool // 网络隔离
	IPC     bool // IPC 隔离
	Cgroup  bool // cgroup 视图隔离（容器看到的 /proc/self/cgroup 以容器根为起点）
	User    bool // uid/gid 隔离（进阶）
}

// Default 返回推荐的一组 namespace 开关。
func Default() Flags {
	return Flags{
		UTS:     true,
		PID:     true,
		Mount:   true,
		Network: true,
		IPC:     true,
		Cgroup:  true,
		User:    false,
	}
}
