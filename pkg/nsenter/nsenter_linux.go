//go:build linux

package nsenter

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// 传给子进程（`mydocker nsexec`）的环境变量名。
const (
	EnvTargetPID = "_MYDOCKER_NSENTER_PID"
	EnvNsList    = "_MYDOCKER_NSENTER_NS"     // 逗号分隔
	EnvCmd       = "_MYDOCKER_NSENTER_CMD"    // 用 EnvCmdSep 分隔的命令段
	EnvCmdSep    = "_MYDOCKER_NSENTER_CMDSEP" // 命令段分隔符，默认 \x1F
	EnvCwd       = "_MYDOCKER_NSENTER_CWD"
)

// SpawnViaSelf 由父进程调用。向后兼容的简易入口：接 os.Stdin/Stdout/Stderr。
// 主要给 `mydocker exec` CLI 使用。
//
// 新调用方应优先使用 Spawn(spec)。
func SpawnViaSelf(target Target, argv []string, tty bool) (exitCode int, err error) {
	spec := ExecSpec{
		Target: target,
		Argv:   argv,
		TTY:    tty,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	if tty {
		spec.Stdin = os.Stdin
	}
	return Spawn(spec)
}

// Spawn 以 ExecSpec 描述的完整参数启动一次 nsenter exec。
// 阻塞等子进程退出，返回其退出码。
func Spawn(spec ExecSpec) (exitCode int, err error) {
	if spec.Target.TargetPID <= 0 {
		return -1, fmt.Errorf("invalid target pid: %d", spec.Target.TargetPID)
	}
	if len(spec.Argv) == 0 {
		return -1, fmt.Errorf("empty argv")
	}
	nss := spec.Target.Namespaces
	if len(nss) == 0 {
		nss = DefaultNamespaces()
	}

	self := spec.InitBinary
	if self == "" {
		var err error
		self, err = os.Executable()
		if err != nil {
			return -1, fmt.Errorf("locate self: %w", err)
		}
	}
	cmd := exec.Command(self, "nsexec")
	// 分隔符：用单元分隔符 \x1F（POSIX shell 命令里罕见），避免 Go exec
	// 对 NUL 的校验。如果 argv 里真出现了 \x1F，再落回 base64。
	sep := "\x1F"
	for _, a := range spec.Argv {
		if strings.ContainsRune(a, '\x1F') {
			sep = "\x02"
			break
		}
	}
	cmd.Env = append(os.Environ(),
		EnvTargetPID+"="+strconv.Itoa(spec.Target.TargetPID),
		EnvNsList+"="+strings.Join(nss, ","),
		EnvCmdSep+"="+sep,
		EnvCmd+"="+strings.Join(spec.Argv, sep),
	)
	if spec.Cwd != "" {
		cmd.Env = append(cmd.Env, EnvCwd+"="+spec.Cwd)
	} else if cwd, _ := os.Getwd(); cwd != "" {
		cmd.Env = append(cmd.Env, EnvCwd+"="+cwd)
	}

	// Route stdio:
	//   - nil stdin  -> no stdin
	//   - nil stdout -> /dev/null equivalent (discard via io.Discard wouldn't be inherited; pass nil)
	//   - nil stderr -> same
	cmd.Stdin = spec.Stdin
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	// If writers are pipes, os/exec handles goroutine copying; if they are
	// *os.File, exec will pass the fd directly.

	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

// EnterAndExec 由子进程（`/proc/self/exe nsexec`）调用。
// 负责 setns 到目标 PID 的各个 namespace，然后 execve 用户命令。
// 永远不返回（成功时是 execve，失败时是 os.Exit）。
func EnterAndExec() error {
	pidStr := os.Getenv(EnvTargetPID)
	nsList := os.Getenv(EnvNsList)
	cmdStr := os.Getenv(EnvCmd)
	cwd := os.Getenv(EnvCwd)

	if pidStr == "" || nsList == "" || cmdStr == "" {
		return fmt.Errorf("nsexec: missing required env vars")
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return fmt.Errorf("nsexec: bad target pid %q", pidStr)
	}
	sep := os.Getenv(EnvCmdSep)
	if sep == "" {
		sep = "\x00" // 兼容老版本（CLI 的 exec）
	}
	argv := strings.Split(cmdStr, sep)
	if len(argv) == 0 || argv[0] == "" {
		return fmt.Errorf("nsexec: empty argv")
	}

	// 锁定到单个 OS 线程，因为 setns 只作用于当前线程
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// setns(CLONE_NEWNS) 要求当前线程在 fs 上没有"伙伴"（不与其它线程共享
	// fs_struct）。Go runtime 是多线程的，所有线程默认共享 fs。先 unshare
	// 本线程的 fs，再 setns 到目标 mnt ns。
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return fmt.Errorf("unshare CLONE_FS: %w", err)
	}

	// 关键：必须在任何 setns 之前把所有 ns fd 打开好。
	// 一旦 setns 到 mnt ns，我们看到的 /proc 就是容器的 /proc，里面不含宿主机
	// 的 pid，后续就打不开其它 ns 文件了。
	nsOrder := strings.Split(nsList, ",")
	type nsFd struct {
		name string
		fd   int
		flag int
	}
	fds := make([]nsFd, 0, len(nsOrder))
	for _, ns := range nsOrder {
		fd, err := openNsFile(pid, ns)
		if err != nil {
			// 关闭已打开的
			for _, f := range fds {
				_ = syscall.Close(f.fd)
			}
			return fmt.Errorf("open ns %s: %w", ns, err)
		}
		fds = append(fds, nsFd{name: ns, fd: fd, flag: nsFlag(ns)})
	}

	// 按顺序 setns；pid 放最后
	for _, f := range fds {
		if err := unix.Setns(f.fd, f.flag); err != nil {
			_ = syscall.Close(f.fd)
			// 剩余未使用的也关掉
			return fmt.Errorf("setns %s: %w", f.name, err)
		}
		_ = syscall.Close(f.fd)
	}

	// 切 cwd（若目标 cwd 存在于容器内则使用，否则回退到 /）
	if cwd != "" {
		if err := syscall.Chdir(cwd); err != nil {
			_ = syscall.Chdir("/")
		}
	} else {
		_ = syscall.Chdir("/")
	}

	// 找可执行文件
	bin := argv[0]
	if !strings.Contains(bin, "/") {
		if p, err := exec.LookPath(bin); err == nil {
			bin = p
		}
	}

	// 进入 pid ns 之后必须再 fork 一次真正的用户命令才会处于新 pid ns
	// 我们把 fork 交给 os/exec.Command：执行它时会 fork+exec，子进程继承当前
	// 所有 namespace，因此它看到的 PID 就是容器里的 PID。
	proc := exec.Command(bin, argv[1:]...)
	proc.Stdin = os.Stdin
	proc.Stdout = os.Stdout
	proc.Stderr = os.Stderr
	// 传递一个干净的 env（去掉我们的控制变量），但保留继承的
	proc.Env = filterEnv(os.Environ())

	if err := proc.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	os.Exit(0)
	return nil
}

func openNsFile(pid int, ns string) (int, error) {
	path := fmt.Sprintf("/proc/%d/ns/%s", pid, ns)
	fd, err := syscall.Open(path, syscall.O_RDONLY, 0)
	if err != nil {
		return -1, err
	}
	return fd, nil
}

// nsFlag 把 "mnt"/"uts"/... 映射成 setns(2) 的 nstype 常量。
// 传 0 也可以（内核自己探测），但显式传更安全。
func nsFlag(ns string) int {
	switch ns {
	case "mnt":
		return unix.CLONE_NEWNS
	case "uts":
		return unix.CLONE_NEWUTS
	case "ipc":
		return unix.CLONE_NEWIPC
	case "net":
		return unix.CLONE_NEWNET
	case "pid":
		return unix.CLONE_NEWPID
	case "user":
		return unix.CLONE_NEWUSER
	case "cgroup":
		return unix.CLONE_NEWCGROUP
	}
	return 0
}

// filterEnv 剔除 nsenter 内部的控制变量，避免泄漏到用户进程。
func filterEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "_MYDOCKER_NSENTER_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
