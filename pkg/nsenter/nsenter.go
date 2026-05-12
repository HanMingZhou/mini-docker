// Package nsenter 封装 "加入已有容器 namespace 后执行命令" 的能力。
//
// 使用 "二段式" 做法：父进程 fork 出子进程（本身是 /proc/self/exe nsexec），
// 子进程一启动就 setns 到目标 pid 的各个 namespace，然后 execve 用户命令。
//
// 为什么要二段式？
//   - setns 加入 mount/net/uts/ipc namespace 对当前线程立刻生效；
//   - 但加入 pid namespace 后，当前进程 *不会* 重新回到"自己是 PID 1"，
//     只有**其后 fork 出来的子进程**才处于新 pid ns 中。
//   - 所以通过再 fork 一次（exec 之前），让真正的用户命令进入新 pid ns。
//
// Go 实现还需要 runtime.LockOSThread，因为 Go 的其它 goroutine 运行在别的 OS
// 线程，不会跟着 setns，会导致不可预测的行为。
package nsenter

import "io"

// Target 描述要加入的 namespace 集合（通过目标容器的 PID 指定）。
type Target struct {
	// TargetPID 是容器 init 进程在**宿主机视角**下的 PID。
	TargetPID int

	// Namespaces 是要加入的 namespace 种类列表，如 []string{"mnt","uts","net","ipc","pid"}。
	// 按这个顺序 setns，pid 必须最后做（因为 pid ns 只对后续 fork 生效）。
	Namespaces []string
}

// ExecSpec 描述一次 nsenter exec 的完整参数。
//
// 该结构同时服务于交互式 exec（终端接 os.Stdin/os.Stdout/os.Stderr）、
// 捕获式 exec（把 stdout/stderr 写到 bytes.Buffer）、以及流式 exec
// （接 HTTP streaming server 的 io.Reader/io.Writer）。
type ExecSpec struct {
	Target Target   // 目标容器 PID + 要进入的 namespace
	Argv   []string // 要执行的命令，Argv[0] 是程序名
	Cwd    string   // 容器内工作目录；空则不切换

	// InitBinary 是承载 `nsexec` 子命令的二进制路径。空则使用 os.Executable()。
	// CLI `mydocker exec` 可留空；CRI 场景（daemon 本身不是 mydocker）必须设置。
	InitBinary string

	// Stdin/Stdout/Stderr 分别接 stdio。nil 表示 /dev/null（stdin）或丢弃（stdout/stderr）。
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// TTY 仅作为语义标记。真正的 TTY 分配由调用方负责（例如使用 pty 库）。
	TTY bool
}

// DefaultNamespaces 返回 exec 场景的典型 namespace 集合。
// 不包含 user namespace；mini-docker 目前也没开 user ns。
func DefaultNamespaces() []string {
	return []string{"ipc", "uts", "net", "mnt", "pid"}
}
