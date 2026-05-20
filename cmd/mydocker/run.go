package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mini-docker/mini-docker/pkg/cgroup"
	"github.com/mini-docker/mini-docker/pkg/container"
	"github.com/mini-docker/mini-docker/pkg/image"
	"github.com/mini-docker/mini-docker/pkg/namespace"
	"github.com/mini-docker/mini-docker/pkg/network"
	"github.com/mini-docker/mini-docker/pkg/rootfs"
	"github.com/mini-docker/mini-docker/pkg/store"
	"github.com/spf13/cobra"
)

// runOptions 聚合 `run` 子命令的所有 flag
type runOptions struct {
	tty      bool
	detach   bool
	rootfs   string
	image    string
	hostname string
	name     string
	memory   string
	cpus     string
	volumes  []string
	envs     []string
	network  string   // bridge (默认) / host / none
	ports    []string // -p host:container[/proto]
	restart  string   // no / always / on-failure
}

func newRunCmd() *cobra.Command {
	var o runOptions
	cmd := &cobra.Command{
		Use:   "run [flags] [--] <image|--rootfs PATH> [cmd...]",
		Short: "Run a command in a new container",
		Long: `Start a new container running the given command.

Use --image to specify the image, or --rootfs for a pre-built directory.
Docker-style shorthand is also accepted: the first positional arg is treated
as the image when --image / --rootfs is not given.`,
		Example: `  mydocker run -it busybox sh                            # docker-style
  mydocker run -it --image busybox -- /bin/sh
  mydocker run -d --image nginx --name web --memory 200m --cpus 0.5 -- nginx -g "daemon off;"
  mydocker run -it --rootfs /tmp/alpine -- /bin/sh`,
		// 允许任意位置参数：第一个可作为 image，其余作为 cmd
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// docker-style: 没有显式 --image / --rootfs 时，把第一个位置参数当 image
			if o.image == "" && o.rootfs == "" && len(args) > 0 {
				o.image = args[0]
				args = args[1:]
			}
			return runContainer(o, args)
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&o.tty, "tty", "t", false, "allocate a TTY and attach stdio (shortcut -it combines -i and -t)")
	f.BoolVarP(&o.detach, "detach", "d", false, "run in background")
	// docker 里 -i 是 interactive，单独开一个 bool，但我们目前没有单独使用
	// 这里提供一个隐藏 alias 让 -it 这种常见写法不报错
	f.BoolP("interactive", "i", false, "keep STDIN open (currently implied by -t)")
	_ = f.MarkHidden("interactive")

	f.StringVar(&o.rootfs, "rootfs", "", "path to a pre-built rootfs (alternative to --image)")
	f.StringVar(&o.image, "image", "", "image name to run (see: mydocker image import)")
	f.StringVar(&o.hostname, "hostname", "mydocker", "container hostname")
	f.StringVar(&o.name, "name", "", "container name (default: random)")
	f.StringVar(&o.memory, "memory", "", "memory limit, e.g. 100m, 1g")
	f.StringVar(&o.cpus, "cpus", "", "CPU quota in cores, e.g. 0.5, 2")
	f.StringArrayVarP(&o.volumes, "volume", "v", nil, "bind mount a volume (host:container[:ro]), repeatable")
	f.StringArrayVarP(&o.envs, "env", "e", nil, "set environment variable KEY=VALUE, repeatable")
	f.StringVar(&o.network, "network", "bridge",
		"network mode: bridge (CNI default), host (share host netns), none (no network)")
	f.StringArrayVarP(&o.ports, "publish", "p", nil,
		"publish a port mapping: [host_ip:]host_port:container_port[/protocol] (repeatable)")
	f.StringVar(&o.restart, "restart", "no",
		"restart policy: no, always, on-failure")
	return cmd
}

func runContainer(o runOptions, cmdArgs []string) error {
	if (o.rootfs == "") == (o.image == "") {
		return fmt.Errorf("exactly one of --image or --rootfs is required")
	}
	if o.tty && o.detach {
		return fmt.Errorf("-t/--tty and -d/--detach cannot be used together")
	}

	memBytes, err := parseMemory(o.memory)
	if err != nil {
		return err
	}
	cpuQuota, cpuPeriod, err := parseCPUs(o.cpus)
	if err != nil {
		return err
	}
	mounts, err := parseVolumes(o.volumes)
	if err != nil {
		return err
	}
	if err := validateEnvs(o.envs); err != nil {
		return err
	}
	ports, err := parsePortMappings(o.ports)
	if err != nil {
		return err
	}
	if len(ports) > 0 && strings.ToLower(o.network) != "bridge" {
		return fmt.Errorf("-p requires --network bridge (current: %s)", o.network)
	}

	st, err := store.New(store.Root())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	id := newID()
	name := o.name
	if name == "" {
		name = id[:6]
	}
	// 原子地检查名字唯一性并预占一个占位记录（跨进程通过 flock 互斥）。
	// 占位后 Name 已登记在 store 里，后面可以做耗时的 rootfs 准备、网络初始化等，
	// 不再担心其它进程同名竞争。
	if err := st.WithLock(func() error {
		if existing, _ := st.Resolve(name); existing != nil {
			return fmt.Errorf("name %q already in use by container %s", name, existing.ID)
		}
		return st.Save(&store.Container{
			ID:        id,
			Name:      name,
			State:     store.StateCreated,
			CreatedAt: time.Now().UTC(),
		})
	}); err != nil {
		return err
	}

	containerDir := st.ContainerDir(id)
	logPath := filepath.Join(containerDir, "container.log")

	// 如果后续任何一步失败，回滚占位记录 + 已挂载的 overlay。
	// 成功路径下 success=true，defer 就变成 no-op。
	success := false
	usingOverlay := false
	defer func() {
		if success {
			return
		}
		// 顺序：先 umount overlay（如果挂了），再删 store 记录
		// （否则 store.Remove 里的 RemoveAll 会因为 merged 是 mount point 而失败，
		// 留下孤儿记录）。
		if usingOverlay {
			_ = image.CleanupRootfs(containerDir)
		}
		_ = st.Remove(id)
	}()

	if err := os.MkdirAll(containerDir, 0755); err != nil {
		return err
	}

	var mergedRoot string
	var imageConfig image.ImageConfig
	if o.image != "" {
		is, err := image.New(store.Root())
		if err != nil {
			return err
		}
		// Pick up default Env / Cmd / WorkingDir from image config
		if m, _ := is.Resolve(o.image); m != nil {
			imageConfig = m.Config
		}
		// 返回mount挂载点
		merged, err := is.PrepareRootfs(o.image, containerDir)
		if err != nil {
			return fmt.Errorf("prepare rootfs from image %s: %w", o.image, err)
		}
		mergedRoot = merged
		usingOverlay = true
	} else {
		mergedRoot = o.rootfs
	}

	if len(mounts) > 0 {
		if err := rootfs.ApplyBindMounts(mergedRoot, mounts); err != nil {
			return fmt.Errorf("apply bind mounts: %w", err)
		}
	}

	rollback := func() {
		// kept for legacy callers below; the deferred cleanup now handles
		// overlay/store cleanup uniformly via success=false.
	}

	// Network mode: bridge / host / none.
	// Default mode: bridge
	netMode := strings.ToLower(strings.TrimSpace(o.network))
	if netMode == "" {
		netMode = "bridge"
	}
	switch netMode {
	case "bridge", "host", "none":
	default:
		return fmt.Errorf("invalid --network %q; want bridge | host | none", o.network)
	}

	nsFlags := namespace.Default()
	if netMode == "host" {
		// host network = share host netns; don't create a new one
		nsFlags.Network = false
	}

	var (
		cniMgr       *network.Manager
		networkSetup func(string) (string, error) // 父进程获取子进程的PID后拼接fmt.Sprintf("/proc/%d/ns/net", cmd.Process.Pid)
	)
	if netMode == "bridge" {
		m, err := network.NewManager("", "", nil) // defaults: /etc/cni/net.d, /opt/cni/bin
		if err != nil {
			return fmt.Errorf("init network manager: %w", err)
		}
		if !m.Ready() {
			return fmt.Errorf("--network bridge requires CNI config in /etc/cni/net.d/ (try 'sudo ./scripts/install-cni.sh')")
		}
		cniMgr = m
		networkSetup = func(netnsPath string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return cniMgr.SetupWithPorts(ctx, id, netnsPath, "eth0", nil, ports)
		}
	}

	// Resolve final Cmd / WorkingDir / Env by merging user flags with image config.
	finalCmd := cmdArgs
	if len(finalCmd) == 0 {
		if len(imageConfig.Entrypoint) > 0 {
			finalCmd = append(finalCmd, imageConfig.Entrypoint...)
		}
		if len(imageConfig.Cmd) > 0 {
			finalCmd = append(finalCmd, imageConfig.Cmd...)
		}
	}
	if len(finalCmd) == 0 {
		return fmt.Errorf("no command specified and image has no default CMD")
	}

	finalEnv := append([]string(nil), imageConfig.Env...)
	for _, e := range o.envs {
		if k := strings.SplitN(e, "=", 2)[0]; k != "" {
			finalEnv = upsertEnv(finalEnv, k, strings.TrimPrefix(e, k+"="))
		}
	}

	finalWD := imageConfig.WorkingDir

	cfg := container.Config{
		ID:           id,
		Name:         name,
		Rootfs:       mergedRoot,
		Hostname:     o.hostname,
		WorkingDir:   finalWD,
		Cmd:          finalCmd,
		Env:          finalEnv,
		TTY:          o.tty,
		Detach:       o.detach,
		LogPath:      logPath,
		Namespaces:   nsFlags,
		NetworkSetup: networkSetup,
		Resources: cgroup.Resources{
			MemoryBytes:     memBytes,
			CPUQuotaMicros:  cpuQuota,
			CPUPeriodMicros: cpuPeriod,
		},
	}

	// Convert ports to store form for persistence
	storePorts := make([]store.PortMapping, 0, len(ports))
	for _, p := range ports {
		storePorts = append(storePorts, store.PortMapping{
			HostPort:      p.HostPort,
			ContainerPort: p.ContainerPort,
			Protocol:      p.Protocol,
			HostIP:        p.HostIP,
		})
	}

	rec := &store.Container{
		ID:            id,
		Name:          name,
		Rootfs:        mergedRoot,
		Hostname:      o.hostname,
		Cmd:           finalCmd,
		Env:           finalEnv,
		State:         store.StateCreated,
		CreatedAt:     time.Now().UTC(),
		LogPath:       logPath,
		NetworkMode:   netMode,
		PortMappings:  storePorts,
		ImageName:     o.image,
		RestartPolicy: o.restart,
		Resources:     cfg.Resources,
	}
	if err := st.Save(rec); err != nil {
		rollback()
		return err
	}

	handle, err := container.Start(cfg)
	if err != nil {
		rollback()
		// store.Remove 由外层 defer 处理
		return err
	}

	rec.PID = handle.PID
	rec.State = store.StateRunning
	rec.CgroupPath = handle.CgroupPath
	rec.NetworkIP = handle.NetworkIP

	// 端口映射：在拿到容器 IP 后添加 iptables DNAT 规则
	if len(ports) > 0 && rec.NetworkIP != "" {
		if err := network.AddPortMappings(rec.NetworkIP, id, ports); err != nil {
			fmt.Fprintf(os.Stderr, "warn: add port mappings: %v\n", err)
		}
	}

	if err := st.Save(rec); err != nil {
		fmt.Fprintf(os.Stderr, "warn: save container state: %v\n", err)
	}

	if o.detach {
		if err := handle.Release(); err != nil {
			fmt.Fprintf(os.Stderr, "warn: release handle: %v\n", err)
		}
		// 如果有 restart policy，启动后台 monitor 进程
		if o.restart == "always" || o.restart == "on-failure" {
			spawnMonitor(id)
		}
		success = true
		fmt.Println(id)
		return nil
	}

	code, werr := handle.Wait()
	rec.State = store.StateExited
	rec.ExitCode = code
	rec.FinishedAt = time.Now().UTC()
	if serr := st.Save(rec); serr != nil {
		fmt.Fprintf(os.Stderr, "warn: save exit state: %v\n", serr)
	}
	// 前台运行正常退出也算成功，保留记录给用户 inspect
	success = true
	if werr != nil {
		return werr
	}
	if code != 0 {
		// 容器非 0 退出。如果容器活了不到 100ms 就死，多半是 init 子进程
		// 在 fork 之后、execve 之前就被某种 LSM/seccomp/namespace 限制拒绝了，
		// 用户日志里看不到任何东西。给个明显的提示。
		lifetime := rec.FinishedAt.Sub(rec.CreatedAt)
		if lifetime < 100*time.Millisecond {
			fmt.Fprintf(os.Stderr,
				"\nerror: container %s exited with code %d after only %v.\n"+
					"This usually means the init process was killed before exec()\n"+
					"by AppArmor / seccomp / nested-userns restrictions, or the host\n"+
					"already has conflicting bridge/veth devices.\n"+
					"Try:\n"+
					"  1. sudo mydocker run --network host ...   (skip CNI)\n"+
					"  2. sudo dmesg | tail -20                  (look for 'DENIED' / 'apparmor')\n"+
					"  3. clean stale state: sudo rm -rf /var/lib/mydocker/containers/* /var/lib/cni/networks/*\n"+
					"  4. on lima/multipass: ensure no docker/containerd/buildkit is running\n",
				rec.ID, code, lifetime)
		}
		os.Exit(code)
	}
	return nil
}

func newID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// parseVolumes 把 "host:container[:ro]" 解析成 rootfs.Mount 切片。
// host 路径会在 Linux 内核做 bind mount 时用到，因此要求是 POSIX 绝对路径。
func parseVolumes(raw []string) ([]rootfs.Mount, error) {
	out := make([]rootfs.Mount, 0, len(raw))
	for _, v := range raw {
		parts := strings.Split(v, ":")
		var mode string
		switch last := parts[len(parts)-1]; last {
		case "ro", "rw":
			mode = last
			parts = parts[:len(parts)-1]
		}
		if len(parts) < 2 {
			return nil, fmt.Errorf("invalid -v %q, want host:container[:ro]", v)
		}
		container := parts[len(parts)-1]
		host := strings.Join(parts[:len(parts)-1], ":")
		if !path.IsAbs(container) {
			return nil, fmt.Errorf("-v %q: container path must be absolute (POSIX)", v)
		}
		if !path.IsAbs(host) {
			return nil, fmt.Errorf("-v %q: host path must be a Linux absolute path", v)
		}
		out = append(out, rootfs.Mount{Source: host, Target: container, ReadOnly: mode == "ro"})
	}
	return out, nil
}

func validateEnvs(envs []string) error {
	for _, e := range envs {
		if !strings.Contains(e, "=") {
			return fmt.Errorf("-e %q: want KEY=VALUE", e)
		}
		k := strings.SplitN(e, "=", 2)[0]
		if k == "" {
			return fmt.Errorf("-e %q: empty key", e)
		}
	}
	return nil
}

func parseMemory(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	s = strings.TrimSpace(strings.ToLower(s))
	mul := int64(1)
	switch s[len(s)-1] {
	case 'k':
		mul = 1 << 10
		s = s[:len(s)-1]
	case 'm':
		mul = 1 << 20
		s = s[:len(s)-1]
	case 'g':
		mul = 1 << 30
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid --memory %q", s)
	}
	return n * mul, nil
}

func parseCPUs(s string) (quota, period int64, err error) {
	if s == "" {
		return 0, 0, nil
	}
	cores, perr := strconv.ParseFloat(s, 64)
	if perr != nil || cores <= 0 || math.IsNaN(cores) || math.IsInf(cores, 0) {
		return 0, 0, fmt.Errorf("invalid --cpus %q", s)
	}
	// 固定值 100_000 是常见默认周期，单位微秒（µs），通常默认值为 100000（即 100 毫秒）
	period = 100_000
	quota = int64(cores * float64(period))
	if quota <= 0 {
		return 0, 0, fmt.Errorf("--cpus %q resolves to 0 quota", s)
	}
	return quota, period, nil
}

// parsePortMappings 解析多个 -p 参数。
// 支持格式：
//
//	8080:80               → hostPort=8080, containerPort=80, protocol=tcp
//	8080:80/udp           → udp
//	127.0.0.1:8080:80     → 绑定到 127.0.0.1
//	127.0.0.1:8080:80/tcp
func parsePortMappings(raw []string) ([]network.PortMapping, error) {
	out := make([]network.PortMapping, 0, len(raw))
	for _, s := range raw {
		pm, err := parseOnePortMapping(s)
		if err != nil {
			return nil, fmt.Errorf("-p %q: %w", s, err)
		}
		out = append(out, pm)
	}
	return out, nil
}

func parseOnePortMapping(s string) (network.PortMapping, error) {
	proto := "tcp"
	if i := strings.LastIndex(s, "/"); i >= 0 {
		proto = strings.ToLower(s[i+1:])
		s = s[:i]
		if proto != "tcp" && proto != "udp" {
			return network.PortMapping{}, fmt.Errorf("protocol must be tcp or udp, got %q", proto)
		}
	}
	parts := strings.Split(s, ":")
	var hostIP, hostPort, ctrPort string
	switch len(parts) {
	case 2:
		hostPort, ctrPort = parts[0], parts[1]
	case 3:
		hostIP, hostPort, ctrPort = parts[0], parts[1], parts[2]
	default:
		return network.PortMapping{}, fmt.Errorf("want [ip:]host:container[/proto]")
	}
	hp, err := strconv.ParseInt(hostPort, 10, 32)
	if err != nil || hp <= 0 || hp > 65535 {
		return network.PortMapping{}, fmt.Errorf("invalid host port %q", hostPort)
	}
	cp, err := strconv.ParseInt(ctrPort, 10, 32)
	if err != nil || cp <= 0 || cp > 65535 {
		return network.PortMapping{}, fmt.Errorf("invalid container port %q", ctrPort)
	}
	return network.PortMapping{
		HostPort:      int32(hp),
		ContainerPort: int32(cp),
		Protocol:      proto,
		HostIP:        hostIP,
	}, nil
}
