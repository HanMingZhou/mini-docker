// Package container 负责容器的创建与启动。
//
// 采用 "父-子双阶段" 模型（runc 和 mydocker 的经典做法）：
//
//  1. 父进程 Start()：使用 exec.Cmd + Cloneflags 拉起一个带新 namespace 的子进程，
//     子进程入口是 /proc/self/exe init（特殊子命令）。
//  2. 子进程 Init()：已在新 namespace 内，做 rootfs.Setup 与 exec 用户命令。
//
// container 包不关心镜像是怎么组装出来的；调用方（CLI / CRI）负责 prepare merged
// 目录并把它作为 Rootfs 传进来。Volume 挂载由调用方在 Start 之前（父进程视角）
// bind mount 到 merged 下。
package container

import (
	"os/exec"
	"sync"

	"github.com/mini-docker/mini-docker/pkg/cgroup"
	"github.com/mini-docker/mini-docker/pkg/namespace"
)

// detachedClosers 保存已 Release 的容器的日志文件句柄，防止被 GC 关闭。
// key 是容器 PID。当容器退出后，调用 CleanupDetached(pid) 释放。
var (
	detachedClosers   = make(map[int][]func() error)
	detachedClosersMu sync.Mutex
)

// Config 是启动一个容器所需的参数。
type Config struct {
	// ID 是容器的唯一 ID（调用方生成，container 包只使用不生成）。
	ID string

	// Name 是容器的人类可读名字，用作 cgroup 目录名。空则用 ID。
	Name string

	// Rootfs 是容器内 / 指向的目录（通常是 overlay 的 merged 目录）。
	Rootfs string

	// Hostname 是容器内 `hostname` 显示的值，空则不设置。
	Hostname string

	// WorkingDir 是容器进程的工作目录（容器内路径）。空则使用 /。
	WorkingDir string

	// Cmd 是容器内要执行的命令。例如 []string{"/bin/sh"}。
	Cmd []string

	// Env 是追加到容器进程的环境变量，格式 "KEY=VALUE"。
	// 容器进程会看到：父 shell env + 这里追加的。
	Env []string

	// TTY 为 true 时把当前终端 stdin/stdout/stderr 接给容器，适合 `run -it`。
	TTY bool

	// Detach 为 true 时父进程不等待容器退出（用于 `run -d`）。
	// 此时 stdout/stderr 会被重定向到 LogPath。
	Detach bool

	// LogPath 是容器 stdout/stderr 的日志文件路径。
	// Detach=true 时必填；否则可以为空（直接用当前终端）。
	LogPath string

	// CRILog 为 true 时按 CRI 规定的日志格式写入 LogPath：
	//   <RFC3339Nano> <stream> <F|P> <msg>
	// CRI daemon 场景必须启用；CLI 的 `run -d` 可以关闭以保持人类可读。
	CRILog bool

	// Namespaces 控制要开启的 namespace。
	Namespaces namespace.Flags

	// JoinNS 指定要加入的已有 namespace 路径（CRI 场景：加入沙箱的 netns/ipc/uts）。
	// key 是 namespace 类型（"net"/"ipc"/"uts"），value 是 /proc/<pid>/ns/<type> 路径。
	// 如果某个 ns 在 JoinNS 中指定了，则 Namespaces 中对应的 CLONE_NEW* 不会被设置。
	JoinNS map[string]string

	// Resources 是要施加的 cgroup 资源限制。0 值表示不限制。
	Resources cgroup.Resources

	// CgroupParent 是 cgroup 父目录（systemd driver 下是 slice 路径）。
	// 由 CRI 的 LinuxContainerConfig.cgroup_parent 传入，CLI 场景一般为空。
	CgroupParent string

	// CgroupDriver 选择 cgroupfs 或 systemd。空则自动探测。
	CgroupDriver cgroup.Driver

	// InitBinary 是容器 init 进程的 host 路径。必须是一个支持 `init` 子命令
	// 的 mini-docker 二进制（即 mydocker）。
	// 留空时 fallback 到 /proc/self/exe —— 这仅在调用方本身是 mydocker 时成立。
	// CRI daemon（mydocker-cri）场景下必须显式传入 mydocker 的路径。
	InitBinary string

	// NetworkSetup 在 namespace 创建后、用户命令 exec 前被调用，通常用于
	// 调用 CNI 插件配置网络（veth + IP + 路由）。如果为 nil，容器里除了
	// loopback 以外没有任何网络接口。
	// 参数 netnsPath 形如 /proc/<init_pid>/ns/net。
	// 返回的 ip 会被调用方保存到元数据。
	NetworkSetup func(netnsPath string) (ip string, err error)
}

// Handle 是一个已启动容器的操作句柄。
type Handle struct {
	cmd    *exec.Cmd
	cgroup cgroup.Manager

	// closers 是父进程在 Start 后需要关闭的资源（日志 sink / CRIWriter 等）。
	// Wait() 或 Release() 中会被调用。
	closers []func() error

	// PID 是子进程的 PID。
	PID int
	// CgroupPath 是该容器的 cgroup 目录路径，便于持久化到 store。
	CgroupPath string
	// NetworkIP 是 CNI 分配的 IPv4，没有则为空。
	NetworkIP string
}

// Start 启动一个容器。成功返回后子进程已进入新 namespace。
// 失败时自行清理（kill 子进程 + 销毁 cgroup）。
func Start(cfg Config) (*Handle, error) {
	return start(cfg)
}

// Wait 阻塞等待容器退出，返回退出码（非 0 也不视为错误）。
// 成功调用后 cgroup 会被销毁。
func (h *Handle) Wait() (exitCode int, err error) {
	return h.wait()
}

// Release 释放 Handle 持有的资源（不 kill 容器，不 destroy cgroup）。
// 适合 detached 模式：父进程退出前调用一次。
// 日志文件句柄会被保存到全局 map 防止 GC 关闭（否则容器写 stdout 时会 SIGPIPE）。
func (h *Handle) Release() error {
	// 把 closers 转移到全局 map，防止 Handle 被 GC 后文件描述符被关闭
	if len(h.closers) > 0 && h.PID > 0 {
		detachedClosersMu.Lock()
		detachedClosers[h.PID] = h.closers
		detachedClosersMu.Unlock()
		h.closers = nil
	}
	return h.release()
}

// CleanupDetached 在容器退出后调用，关闭其日志文件句柄并从全局 map 中移除。
func CleanupDetached(pid int) {
	detachedClosersMu.Lock()
	closers := detachedClosers[pid]
	delete(detachedClosers, pid)
	detachedClosersMu.Unlock()
	for _, c := range closers {
		_ = c()
	}
}

// Init 由子进程（`mydocker init`）调用，用户不要直接调用。
func Init() error {
	return initProcess()
}
