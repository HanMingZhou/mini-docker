//go:build linux

package container

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mini-docker/mini-docker/pkg/cgroup"
	"github.com/mini-docker/mini-docker/pkg/rootfs"
	"golang.org/x/sys/unix"
)

// 父子之间通过一对 pipe 传递 "启动指令"（JSON 编码的 initPayload）。
// FD 3 = pipe 的读端（由父进程通过 ExtraFiles 注入子进程）。
const initPipeFD = 3

// initPayload 是父进程通过 pipe 告诉子进程的启动参数。
type initPayload struct {
	Rootfs     string            `json:"rootfs"`
	Hostname   string            `json:"hostname"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Cmd        []string          `json:"cmd"`
	Env        []string          `json:"env"`
	JoinNS     map[string]string `json:"join_ns,omitempty"` // ns type -> /proc/<pid>/ns/<type>
}

func start(cfg Config) (*Handle, error) {
	if len(cfg.Cmd) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	if cfg.Rootfs == "" {
		return nil, fmt.Errorf("empty rootfs")
	}
	if cfg.Detach && cfg.LogPath == "" {
		return nil, fmt.Errorf("detach mode requires LogPath")
	}

	cgName := cfg.Name
	if cgName == "" {
		cgName = cfg.ID
	}
	if cgName == "" {
		return nil, fmt.Errorf("cgroup name cannot be derived: both Name and ID are empty")
	}

	// 父子通信管道
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create init pipe: %w", err)
	}
	defer r.Close()

	// 计算 clone flags：如果某个 ns 要 join 已有的，就不创建新的
	cloneFlags := cfg.Namespaces.CloneFlags()
	if cfg.JoinNS != nil {
		for nsType := range cfg.JoinNS {
			cloneFlags = clearCloneFlag(cloneFlags, nsType)
		}
	}

	cmd := exec.Command(initBinary(cfg), "init")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: cloneFlags,
	}
	// 把管道读端塞给子进程
	cmd.ExtraFiles = []*os.File{r}

	// stdio：detach -> 日志文件；前台 tty -> 当前终端；否则继承 stdout/err
	var extraClosers []func() error
	if cfg.Detach {
		logDir := filepath.Dir(cfg.LogPath)
		if err := os.MkdirAll(logDir, 0755); err != nil {
			_ = w.Close()
			return nil, fmt.Errorf("mkdir log dir: %w", err)
		}
		if cfg.CRILog {
			// 直接用文件作为 stdout/stderr，避免 Go 的 pipe 机制。
			// CRI 日志格式由一个包装进程处理（后续优化），当前先保证容器稳定。
			logFile, err := os.OpenFile(cfg.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				_ = w.Close()
				return nil, fmt.Errorf("open log file: %w", err)
			}
			cmd.Stdin = nil
			cmd.Stdout = logFile
			cmd.Stderr = logFile
			extraClosers = append(extraClosers, logFile.Close)
		} else {
			logFile, err := os.OpenFile(cfg.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				_ = w.Close()
				return nil, fmt.Errorf("open log file: %w", err)
			}
			cmd.Stdin = nil
			cmd.Stdout = logFile
			cmd.Stderr = logFile
			extraClosers = append(extraClosers, logFile.Close)
		}
	} else if cfg.TTY {
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		cmd.Stdin = nil
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}

	if err := cmd.Start(); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("start init process: %w", err)
	}

	// 创建 cgroup（写文件，比如 /sys/fs/cgroup/mydocker/<name>/）
	cg, err := cgroup.NewWithConfig(cgroup.Config{
		Name:   cgName,
		Parent: cfg.CgroupParent,
		Driver: cfg.CgroupDriver,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		_ = w.Close()
		return nil, fmt.Errorf("new cgroup manager: %w", err)
	}
	// Apply 写资源限制（内存上限、CPU 配额）
	if err := cg.Apply(cfg.Resources); err != nil {
		_ = cmd.Process.Kill()
		_ = cg.Destroy()
		_ = w.Close()
		return nil, fmt.Errorf("apply cgroup: %w", err)
	}
	// AddProc 把子进程 PID 写进 cgroup.procs 文件 —— 这一步必须在父进程做，因为子进程在新 PID namespace 里看不到自己的"真实 PID
	if err := cg.AddProc(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cg.Destroy()
		_ = w.Close()
		return nil, fmt.Errorf("add pid %d to cgroup: %w", cmd.Process.Pid, err)
	}

	// 网络配置：此时 cmd 已启动，netns 存在；init 子进程正阻塞在 pipe 读上，
	// 还没 exec 用户命令。这是调用 CNI ADD 的最佳时机。
	/*
		子进程已经有了独立的 netns（在 /proc/<pid>/ns/net），但里面只有 lo（loopback），没有真正的网卡。
		父进程调 CNI 插件（比如 bridge），让插件在子进程的 netns 里造一对 veth、配 IP、加路由。
		之所以必须现在做：子进程还没 exec，netns 是空的、安全的。等用户命令跑起来再改网络就晚了。
	*/
	var networkIP string
	if cfg.NetworkSetup != nil {
		netnsPath := fmt.Sprintf("/proc/%d/ns/net", cmd.Process.Pid)
		ip, err := cfg.NetworkSetup(netnsPath)
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cg.Destroy()
			_ = w.Close()
			return nil, fmt.Errorf("network setup: %w", err)
		}
		networkIP = ip
	}

	payload := initPayload{
		Rootfs:     cfg.Rootfs,
		Hostname:   cfg.Hostname,
		WorkingDir: cfg.WorkingDir,
		Cmd:        cfg.Cmd,
		Env:        cfg.Env,
		JoinNS:     cfg.JoinNS,
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		_ = cmd.Process.Kill()
		_ = cg.Destroy()
		_ = w.Close()
		return nil, fmt.Errorf("send init payload: %w", err)
	}
	_ = w.Close()

	return &Handle{
		cmd:        cmd,
		cgroup:     cg,
		closers:    extraClosers,
		PID:        cmd.Process.Pid,
		CgroupPath: cg.Path(),
		NetworkIP:  networkIP,
	}, nil
}

func (h *Handle) wait() (int, error) {
	err := h.cmd.Wait()
	// Close log sinks / CRIWriters after Wait: by now the copier goroutines
	// spawned by os/exec have drained the child's pipes and returned.
	for _, c := range h.closers {
		if cerr := c(); cerr != nil {
			fmt.Fprintf(os.Stderr, "close: %v\n", cerr)
		}
	}
	if derr := h.cgroup.Destroy(); derr != nil {
		fmt.Fprintf(os.Stderr, "cgroup destroy: %v\n", derr)
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return -1, fmt.Errorf("wait: %w", err)
	}
	return 0, nil
}

func (h *Handle) release() error {
	if h.cmd != nil && h.cmd.Process != nil {
		return h.cmd.Process.Release()
	}
	return nil
}

// initProcess 在**新 namespace** 的子进程里执行。
func initProcess() error {
	pipe := os.NewFile(uintptr(initPipeFD), "init-pipe")
	if pipe == nil {
		return fmt.Errorf("init pipe not inherited")
	}
	defer pipe.Close()

	raw, err := io.ReadAll(pipe)
	if err != nil {
		return fmt.Errorf("read init payload: %w", err)
	}
	var payload initPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("decode init payload: %w", err)
	}

	// 加入已有 namespace（CRI 场景：加入沙箱的 netns/ipc/uts）
	if len(payload.JoinNS) > 0 {
		if err := joinNamespaces(payload.JoinNS); err != nil {
			return fmt.Errorf("join namespaces: %w", err)
		}
	}

	if payload.Hostname != "" {
		if err := syscall.Sethostname([]byte(payload.Hostname)); err != nil {
			return fmt.Errorf("sethostname: %w", err)
		}
	}

	// pivot_root + /proc /sys /dev（extraMounts 已在父进程预挂到 merged）
	if err := rootfs.Setup(payload.Rootfs, nil); err != nil {
		return fmt.Errorf("rootfs setup: %w", err)
	}

	// 切到容器内工作目录（pivot_root 之后我们在 /）
	if payload.WorkingDir != "" {
		if err := syscall.Chdir(payload.WorkingDir); err != nil {
			// 不存在就回落到 /，不要因为一个 cwd 就失败
			fmt.Fprintf(os.Stderr, "warn: chdir %q: %v; falling back to /\n", payload.WorkingDir, err)
		}
	}

	// 合并环境变量：父 env + payload.Env（后者优先覆盖）
	envMap := make(map[string]string)
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			envMap[kv[:i]] = kv[i+1:]
		}
	}
	for _, kv := range payload.Env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			envMap[kv[:i]] = kv[i+1:]
		}
	}
	envSlice := make([]string, 0, len(envMap))
	for k, v := range envMap {
		envSlice = append(envSlice, k+"="+v)
	}

	bin, err := exec.LookPath(payload.Cmd[0])
	if err != nil {
		if strings.Contains(payload.Cmd[0], "/") {
			bin = payload.Cmd[0]
		} else {
			return fmt.Errorf("look up %q: %w", payload.Cmd[0], err)
		}
	}
	if err := syscall.Exec(bin, payload.Cmd, envSlice); err != nil {
		return fmt.Errorf("exec %q: %w", bin, err)
	}
	return nil
}

// joinNamespaces 使用 setns 加入指定的 namespace。
func joinNamespaces(nsMap map[string]string) error {
	// 顺序：net, ipc, uts（pid 不在这里处理，mount 也不——我们自己做 pivot_root）
	order := []string{"net", "ipc", "uts"}
	for _, ns := range order {
		path, ok := nsMap[ns]
		if !ok {
			continue
		}
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open %s (%s): %w", ns, path, err)
		}
		if err := unix.Setns(fd, nsCloneFlag(ns)); err != nil {
			_ = unix.Close(fd)
			return fmt.Errorf("setns %s: %w", ns, err)
		}
		_ = unix.Close(fd)
	}
	return nil
}

func nsCloneFlag(ns string) int {
	switch ns {
	case "net":
		return syscall.CLONE_NEWNET
	case "ipc":
		return syscall.CLONE_NEWIPC
	case "uts":
		return syscall.CLONE_NEWUTS
	case "pid":
		return syscall.CLONE_NEWPID
	case "mnt":
		return 0x00020000 // CLONE_NEWNS
	}
	return 0
}

// clearCloneFlag removes the CLONE_NEW* flag for the given namespace type.
func clearCloneFlag(flags uintptr, ns string) uintptr {
	switch ns {
	case "net":
		return flags &^ uintptr(syscall.CLONE_NEWNET)
	case "ipc":
		return flags &^ uintptr(syscall.CLONE_NEWIPC)
	case "uts":
		return flags &^ uintptr(syscall.CLONE_NEWUTS)
	case "pid":
		return flags &^ uintptr(syscall.CLONE_NEWPID)
	case "mnt":
		return flags &^ uintptr(syscall.CLONE_NEWNS)
	}
	return flags
}

// initBinary 选择执行 "<bin> init" 的二进制：
//   - 显式传入的 cfg.InitBinary 优先
//   - 否则回退到 /proc/self/exe，要求调用方本身是一个支持 `init` 子命令的
//     mydocker 二进制（CLI `mydocker run` 即如此）
func initBinary(cfg Config) string {
	if cfg.InitBinary != "" {
		return cfg.InitBinary
	}
	return "/proc/self/exe"
}
