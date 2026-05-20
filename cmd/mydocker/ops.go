package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/mini-docker/mini-docker/pkg/image"
	"github.com/mini-docker/mini-docker/pkg/network"
	"github.com/mini-docker/mini-docker/pkg/store"
)

func newPsCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List containers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return listContainers(all)
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false,
		"show all containers (reserved; currently all are always shown)")
	return cmd
}

func listContainers(_ bool) error {
	st, err := store.New(store.Root())
	if err != nil {
		return err
	}
	list, err := st.List()
	if err != nil {
		return err
	}
	for _, c := range list {
		if c.State == store.StateRunning && !pidAlive(c.PID) {
			c.State = store.StateExited
			c.FinishedAt = time.Now().UTC()
			_ = st.Save(c)
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CONTAINER ID\tNAME\tSTATE\tPID\tIP\tPORTS\tCREATED\tCOMMAND")
	for _, c := range list {
		ip := c.NetworkIP
		if ip == "" {
			if c.NetworkMode == "host" {
				ip = "host"
			} else {
				ip = "-"
			}
		}
		ports := "-"
		if len(c.PortMappings) > 0 {
			parts := make([]string, 0, len(c.PortMappings))
			for _, p := range c.PortMappings {
				seg := fmt.Sprintf("%d->%d/%s", p.HostPort, p.ContainerPort, p.Protocol)
				if p.HostIP != "" {
					seg = fmt.Sprintf("%s:%s", p.HostIP, seg)
				}
				parts = append(parts, seg)
			}
			ports = strings.Join(parts, ",")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			c.ID, c.Name, c.State, c.PID, ip, ports, humanAge(c.CreatedAt), strings.Join(c.Cmd, " "))
	}
	return w.Flush()
}

func newLogsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logs <id|name>",
		Short: "Show container logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := store.New(store.Root())
			if err != nil {
				return err
			}
			c, err := st.Resolve(args[0])
			if err != nil {
				return err
			}
			f, err := os.Open(c.LogPath)
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			defer f.Close()
			_, err = io.Copy(os.Stdout, f)
			return err
		},
	}
}

func newStopCmd() *cobra.Command {
	var timeout int
	cmd := &cobra.Command{
		Use:   "stop <id|name>",
		Short: "Stop a running container (SIGTERM then SIGKILL)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return stopContainer(args[0], timeout)
		},
	}
	cmd.Flags().IntVarP(&timeout, "timeout", "t", 10, "grace period in seconds before SIGKILL")
	return cmd
}

func stopContainer(ref string, timeout int) error {
	st, err := store.New(store.Root())
	if err != nil {
		return err
	}
	c, err := st.Resolve(ref)
	if err != nil {
		return err
	}
	if c.State != store.StateRunning && c.State != store.StatePaused {
		fmt.Fprintf(os.Stderr, "container %s is not running\n", c.ID)
		return nil
	}
	if !pidAlive(c.PID) {
		fmt.Fprintf(os.Stderr, "container %s is not running\n", c.ID)
		return nil
	}

	// 如果是 paused，先 unfreeze，否则 SIGTERM 不会被处理
	if c.State == store.StatePaused {
		_ = pauseContainer(c.ID, false) // best-effort
	}

	// Teardown CNI before killing the process: libcni needs the netns
	// (via /proc/<pid>/ns/net) to still exist so it can enter it and rip
	// veth interfaces. 幂等，失败只打 warn。
	teardownNetwork(c)

	if err := sendTerm(c.PID); err != nil {
		return fmt.Errorf("send SIGTERM to %d: %w", c.PID, err)
	}

	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(c.PID) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if pidAlive(c.PID) {
		_ = sendKill(c.PID)
	}

	c.State = store.StateExited
	c.FinishedAt = time.Now().UTC()
	return st.Save(c)
}

func newRmCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rm <id|name>",
		Short: "Remove a stopped container",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return removeContainer(args[0], force)
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "force remove a running container")
	return cmd
}

func removeContainer(ref string, force bool) error {
	st, err := store.New(store.Root())
	if err != nil {
		return err
	}
	c, err := st.Resolve(ref)
	if err != nil {
		return err
	}

	if (c.State == store.StateRunning || c.State == store.StatePaused) && pidAlive(c.PID) {
		if !force {
			return fmt.Errorf("container %s is %s; use --force or stop it first", c.ID, c.State)
		}
		// Unfreeze paused container so SIGKILL can actually take effect
		if c.State == store.StatePaused {
			_ = pauseContainer(c.ID, false)
		}
		// 先 CNI DEL 再 kill，否则 netns 没了插件回收不到 veth
		teardownNetwork(c)
		_ = sendKill(c.PID)
		time.Sleep(200 * time.Millisecond)
	} else {
		// 容器已经不再 Running（Exited / Created），但它的 CNI 留下的 iptables
		// 规则、IPAM 占用、veth peer 等仍可能残留——必须 best-effort 清理一次。
		teardownNetwork(c)
	}

	if c.CgroupPath != "" {
		if err := os.Remove(c.CgroupPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warn: remove cgroup %s: %v\n", c.CgroupPath, err)
		}
	}

	if err := image.CleanupRootfs(st.ContainerDir(c.ID)); err != nil {
		fmt.Fprintf(os.Stderr, "warn: cleanup rootfs %s: %v\n", c.ID, err)
	}

	if err := st.Remove(c.ID); err != nil {
		return err
	}
	fmt.Println(c.ID)
	return nil
}

// teardownNetwork 在容器被停止之前调用 CNI DEL，释放 IP、veth、端口映射。
// libcni 内部把首次 ADD 的参数缓存到 /var/lib/cni/cache/，DEL 时会自动复用，
// 所以这里不需要再显式传 ports / capabilityArgs。
// 幂等：没网络 / CNI 未加载 / 已 down / 容器已退出 都不会报错。
func teardownNetwork(c *store.Container) {
	if c.NetworkMode != "bridge" {
		return
	}

	// 清理端口映射 iptables 规则（在 CNI DEL 之前，因为 DEL 会删 veth）
	if len(c.PortMappings) > 0 && c.NetworkIP != "" {
		netPorts := storePortsToNetwork(c.PortMappings)
		network.RemovePortMappings(c.NetworkIP, c.ID, netPorts)
	}

	mgr, err := network.NewManager("", "", nil)
	if err != nil || !mgr.Ready() {
		return
	}

	// netns 路径：进程还活着就用 /proc/<pid>/ns/net；否则传空字符串，
	// libcni 会跳过进入 netns 的步骤（仅清理 host 侧 iptables / IPAM 等）。
	netns := ""
	if c.PID > 0 && pidAlive(c.PID) {
		candidate := fmt.Sprintf("/proc/%d/ns/net", c.PID)
		if _, err := os.Stat(candidate); err == nil {
			netns = candidate
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mgr.Teardown(ctx, c.ID, netns, "eth0"); err != nil {
		fmt.Fprintf(os.Stderr, "warn: cni teardown %s: %v\n", c.ID, err)
	}
}

// storePortsToNetwork converts store.PortMapping slice to network.PortMapping slice.
func storePortsToNetwork(sp []store.PortMapping) []network.PortMapping {
	out := make([]network.PortMapping, 0, len(sp))
	for _, p := range sp {
		out = append(out, network.PortMapping{
			HostPort:      p.HostPort,
			ContainerPort: p.ContainerPort,
			Protocol:      p.Protocol,
		})
	}
	return out
}

// humanAge 返回 "3m ago" / "2h ago" 这种粗略时长。
func humanAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
