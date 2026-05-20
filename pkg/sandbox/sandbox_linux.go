//go:build linux

package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// start 拉起 pause 进程，使之运行在独立的 net/uts/ipc namespace 内。
// 注意：
//   - 不开 PID namespace。Pod 内业务容器默认 *不* 共享 PID ns。
//     如果上层要求共享，由 CRI 层在 CreateContainer 时将业务容器 setns 到
//     sb.PID 的 pid ns（后续实现）。
//   - pause 进程不做 pivot_root；它只是个 namespace 载体。
//   - pause 用 Setsid 脱离父进程的 session，否则 daemon 通过 shell 启动时
//     shell 退出会让 systemd 顺带清理整个 session scope，把 pause 也杀掉。
//
// hostNetwork 模式：不启动 pause 进程，直接使用 PID 1 的 namespace。
// 这样 sandbox 永远 Ready（PID 1 不会死），容器 join 宿主机 namespace。
func (m *Manager) start(md Metadata, opts StartOptions) (*Sandbox, error) {
	if md.Name == "" || md.Namespace == "" || md.UID == "" {
		return nil, fmt.Errorf("sandbox metadata requires name, namespace, uid; got %+v", md)
	}

	id := newSandboxID()
	sbDir := m.SandboxDir(id)
	if err := os.MkdirAll(sbDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir sandbox dir: %w", err)
	}

	// hostNetwork Pod：不需要 pause 进程，直接用 PID 1 的 namespace
	if opts.HostNetwork {
		sb := &Sandbox{
			ID:           id,
			Metadata:     md,
			State:        StateReady,
			PID:          1, // PID 1 (systemd) 永远活着
			NetnsPath:    "/proc/1/ns/net",
			CgroupParent: opts.CgroupParent,
			CreatedAt:    time.Now().UTC(),
			LogDir:       opts.LogDir,
			Labels:       opts.Labels,
			Annotations:  opts.Annotations,
		}
		if err := m.save(sb); err != nil {
			_ = os.RemoveAll(sbDir)
			return nil, fmt.Errorf("save sandbox: %w", err)
		}
		return sb, nil
	}

	// 启动 pause 进程（核心）
	cmd := exec.Command(m.selfExe, "sandbox-pause")
	// CLONE_NEWUTS	创建新的 UTS namespace——容器可以有自己的 hostname
	// CLONE_NEWIPC	创建新的 IPC namespace——隔离 System V IPC、POSIX 消息队列
	// CLONE_NEWNET	创建新的 Network namespace——容器有独立的网卡、IP、路由表
	// Setsid: true	让 pause 成为新 session leader，脱离父进程的终端会话。否则父 shell 退出时 systemd 会清理整个 session，连带杀死 pause
	cloneFlags := uintptr(syscall.CLONE_NEWUTS | syscall.CLONE_NEWIPC | syscall.CLONE_NEWNET)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: cloneFlags,
		Setsid:     true,
	}
	// 把 pause 的 stdio 丢到 /dev/null，避免继承父进程的终端
	devNull, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		_ = os.RemoveAll(sbDir)
		return nil, fmt.Errorf("open /dev/null: %w", err)
	}
	defer devNull.Close()
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(sbDir)
		return nil, fmt.Errorf("start pause: %w", err)
	}
	pid := cmd.Process.Pid

	// Release 不 wait，由 PID 1 收养（父进程 mydocker-cri 常驻，也能手动 wait）
	if err := cmd.Process.Release(); err != nil {
		// 非致命
		fmt.Fprintf(os.Stderr, "warn: release pause process: %v\n", err)
	}

	sb := &Sandbox{
		ID:           id,
		Metadata:     md,
		State:        StateReady,
		PID:          pid,
		NetnsPath:    fmt.Sprintf("/proc/%d/ns/net", pid),
		CgroupParent: opts.CgroupParent,
		CreatedAt:    time.Now().UTC(),
		LogDir:       opts.LogDir,
		Labels:       opts.Labels,
		Annotations:  opts.Annotations,
	}

	// 基础校验：/proc/<pid>/ns/net 应可 stat（在调 CNI 之前）
	if _, statErr := os.Stat(sb.NetnsPath); statErr != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_ = os.RemoveAll(sbDir)
		return nil, fmt.Errorf("stat netns %s: %w", sb.NetnsPath, statErr)
	}

	// CNI ADD：给沙箱分配 IP、配置 veth。失败则回滚整个 sandbox。
	if m.netHook != nil {
		ip, err := m.netHook.Setup(sb.ID, sb.NetnsPath)
		if err != nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			_ = os.RemoveAll(sbDir)
			return nil, fmt.Errorf("cni setup: %w", err)
		}
		sb.IP = ip
	}

	if err := m.save(sb); err != nil {
		// pause 已起来但元数据写失败——尽力回滚
		if m.netHook != nil {
			_ = m.netHook.Teardown(sb.ID, sb.NetnsPath)
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_ = os.RemoveAll(sbDir)
		return nil, fmt.Errorf("save sandbox: %w", err)
	}
	return sb, nil
}

func (m *Manager) stop(id string) error {
	sb, err := m.loadByID(id)
	if err != nil {
		return err
	}

	// hostNetwork sandbox 用 PID 1，不需要 kill
	if sb.PID == 1 {
		sb.State = StateNotReady
		if sb.FinishedAt.IsZero() {
			sb.FinishedAt = time.Now().UTC()
		}
		return m.save(sb)
	}

	if !pidAlive(sb.PID) {
		// pause 已经死了：netns 已随之消失，CNI Teardown 也就不再必要。
		// 直接把状态落盘标记 NotReady，避免 crictl 卡在 DeadlineExceeded。
		sb.State = StateNotReady
		if sb.FinishedAt.IsZero() {
			sb.FinishedAt = time.Now().UTC()
		}
		return m.save(sb)
	}

	// CNI DEL 必须在 pause 退出之前调用：插件需要 netns 还存在才能回收 veth。
	if m.netHook != nil {
		if err := m.netHook.Teardown(sb.ID, sb.NetnsPath); err != nil {
			fmt.Fprintf(os.Stderr, "warn: cni teardown: %v\n", err)
		}
	}

	// pause 对 SIGTERM 响应很快，超时兜底 SIGKILL
	_ = syscall.Kill(sb.PID, syscall.SIGTERM)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(sb.PID) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(sb.PID) {
		_ = syscall.Kill(sb.PID, syscall.SIGKILL)
	}

	sb.State = StateNotReady
	sb.FinishedAt = time.Now().UTC()
	return m.save(sb)
}

// pidAlive 是 sandbox 包内的私有实现（package-level），
// 不依赖 cmd/mydocker 中同名函数。
//
// 用 kill(pid, 0) 探活：
//   - nil   → 进程存在
//   - EPERM → 进程存在但当前调用方无权限（视为存活）
//   - ESRCH → 不存在
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// newSandboxID 生成 12 位十六进制。
func newSandboxID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// 仅用于避免 import 清理：sandbox.go 用到了 filepath 但分平台文件里没。
