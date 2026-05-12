package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mini-docker/mini-docker/pkg/store"
)

// newMonitorCmd 返回一个隐藏的 `_monitor` 子命令。
// 由 `run -d --restart always` 自动 fork 出来，负责监控容器退出并重启。
// 它以 Setsid 脱离终端，作为独立后台进程运行。
func newMonitorCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "_monitor",
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return monitorLoop(args[0])
		},
	}
}

// spawnMonitor fork 一个后台 monitor 进程。
func spawnMonitor(containerID string) {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: cannot spawn monitor: %v\n", err)
		return
	}
	cmd := exec.Command(self, "_monitor", containerID)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "warn: spawn monitor: %v\n", err)
		return
	}
	_ = cmd.Process.Release()
}

// monitorLoop 是 monitor 进程的主循环。
// 它轮询容器状态，当容器退出时重新启动它。
// 退出条件：
//   - 容器被 `mydocker rm` 删除（store 里找不到了）
//   - RestartPolicy 是 "on-failure" 且退出码为 0
//   - 连续重启失败超过 maxRetries 次
const maxRetries = 5

func monitorLoop(containerID string) error {
	backoff := time.Second
	failures := 0

	for {
		st, err := store.New(store.Root())
		if err != nil {
			return err
		}
		c, err := st.Resolve(containerID)
		if err != nil {
			// 容器已被删除，退出 monitor
			return nil
		}

		// 如果容器还在运行，等一会再检查
		if c.State == store.StateRunning && pidAlive(c.PID) {
			time.Sleep(2 * time.Second)
			continue
		}

		// 容器已退出
		if c.RestartPolicy == "on-failure" && c.ExitCode == 0 {
			// 正常退出，不重启
			return nil
		}
		if c.RestartPolicy != "always" && c.RestartPolicy != "on-failure" {
			return nil
		}

		// 等待 backoff 后重启
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}

		// 重启：重新调用 container.Start
		if err := restartContainer(c); err != nil {
			failures++
			fmt.Fprintf(os.Stderr, "monitor: restart %s failed (%d/%d): %v\n",
				containerID, failures, maxRetries, err)
			if failures >= maxRetries {
				return fmt.Errorf("max retries exceeded")
			}
			continue
		}
		// 重启成功，重置 backoff
		backoff = time.Second
		failures = 0
	}
}

// restartContainer 重新启动一个已退出的容器。
// 复用原来的配置（rootfs 还在、cgroup 名不变）。
func restartContainer(c *store.Container) error {
	// 简化实现：用 mydocker 自己的 run 逻辑太复杂（需要重新组装 overlay 等）。
	// 最简方案：直接 exec `mydocker run` 用相同参数。
	// 但我们没有保存完整的原始参数。
	//
	// 更实际的方案：直接调 container.Start 用 store 里保存的信息。
	// 但 rootfs（overlay merged）在容器退出后可能已经 umount 了。
	//
	// 教学级方案：如果 rootfs 还在（没被 rm），直接重新 Start。
	// 如果 rootfs 不在了（被 rm 了），放弃。

	st, err := store.New(store.Root())
	if err != nil {
		return err
	}

	// 检查 rootfs 是否还在
	if _, err := os.Stat(c.Rootfs); err != nil {
		return fmt.Errorf("rootfs gone: %w", err)
	}

	// 用 exec.Command 重新跑容器进程（最简方案）
	self, err := os.Executable()
	if err != nil {
		return err
	}

	// 构造最小的 run 命令
	args := []string{"run", "-d", "--rootfs", c.Rootfs, "--name", c.Name + "-r"}
	if c.Hostname != "" {
		args = append(args, "--hostname", c.Hostname)
	}
	args = append(args, "--network", "none") // 重启时网络状态复杂，先用 none
	args = append(args, "--")
	args = append(args, c.Cmd...)

	cmd := exec.Command(self, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("re-run: %w", err)
	}

	// 更新原容器状态为 Running（简化：实际上是新容器）
	c.State = store.StateRunning
	return st.Save(c)
}
