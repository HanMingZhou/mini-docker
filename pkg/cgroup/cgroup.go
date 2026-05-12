// Package cgroup 提供对 Linux cgroups 的简单封装。
//
// 支持两种 driver（与 runc / containerd / kubelet 的概念对齐）：
//
//   - cgroupfs：直接 mkdir + 写文件到 /sys/fs/cgroup/...
//   - systemd：通过 systemd D-Bus 创建 transient scope，让 systemd 代管 cgroup
//
// Kubelet 默认用 systemd driver；想接 Kubelet 就必须支持它。
// 两种 driver 必须全节点一致，否则 Pod 的 QoS 层级对不上。
//
// 目前只实现 cgroups v2（大多数现代发行版默认）。v1 留作后续兼容。
package cgroup

import "errors"

// Driver 标识 cgroup 管理方式。
type Driver string

const (
	// DriverCgroupfs 直接操作 /sys/fs/cgroup。
	DriverCgroupfs Driver = "cgroupfs"
	// DriverSystemd 通过 systemd 的 StartTransientUnit 创建 scope/slice。
	DriverSystemd Driver = "systemd"
)

// Resources 描述对容器进程施加的资源限制。
// 空字段表示不限制该维度。
type Resources struct {
	// MemoryBytes 是内存上限，单位 byte。0 表示不限制。
	// 对应 cgroup v2 的 memory.max / systemd 的 MemoryMax。
	MemoryBytes int64

	// CPUQuotaMicros / CPUPeriodMicros 对应 cgroup v2 的 cpu.max：
	//   "<quota> <period>"，意为每个 period 内最多用 quota 微秒 CPU。
	// 例如 50000 / 100000 = 0.5 核。0 表示不限制。
	CPUQuotaMicros  int64
	CPUPeriodMicros int64
}

// Config 是创建 Manager 的参数。
type Config struct {
	// Driver 选择 cgroupfs 还是 systemd。空则自动探测（有 systemd 用 systemd）。
	Driver Driver

	// Name 是叶子 cgroup 的名字。
	//   - cgroupfs 下直接作为 /sys/fs/cgroup/<Parent>/<Name> 的 Name 段。
	//   - systemd 下会被包装成 `<Name>.scope`（要求是合法 systemd unit 名）。
	Name string

	// Parent 是父 cgroup。
	//   - cgroupfs: 相对 /sys/fs/cgroup 的路径，空则放在根下。
	//   - systemd: 形如 "kubepods.slice/kubepods-besteffort.slice/..."，
	//     由 Kubelet 在 CRI 请求中通过 LinuxContainerConfig.cgroup_parent 传进来；
	//     空则放在 system.slice 下（仅适合独立调试）。
	Parent string
}

// Manager 管理一个 cgroup 的生命周期。
type Manager interface {
	// Apply 创建 cgroup 并应用 Resources 中的限制。
	// 对于 systemd driver，这一步会调 StartTransientUnit 直接把 pid 作为
	// 新 scope 的入口进程——因此 AddProc 可以在 Apply 之后立即为 no-op。
	Apply(r Resources) error

	// AddProc 把指定 pid 加入 cgroup。
	// 对 systemd driver，如果 pid 已经是 scope 的入口则是 no-op。
	AddProc(pid int) error

	// Destroy 移除 cgroup。
	// cgroupfs 下是 rmdir；systemd 下是 StopUnit。
	Destroy() error

	// Path 返回 cgroup 在宿主机上的绝对路径，便于排错。
	Path() string

	// Freeze 冻结 / 解冻 cgroup 内的所有进程（cgroup v2 freezer）。
	// freeze=true 冻结，false 解冻。幂等。
	Freeze(freeze bool) error
}

// NewManager 兼容旧调用：NewManager("name") 等价于 NewWithConfig(Config{Name: "name"}).
// 使用 cgroupfs driver，Parent 为空。
func NewManager(name string) (Manager, error) {
	return NewWithConfig(Config{Name: name, Driver: DriverCgroupfs})
}

// NewWithConfig 按 Config 创建 Manager。
func NewWithConfig(cfg Config) (Manager, error) {
	if cfg.Name == "" {
		return nil, errors.New("cgroup name is empty")
	}
	if cfg.Driver == "" {
		cfg.Driver = autoDetectDriver()
	}
	return newManager(cfg)
}

// autoDetectDriver: 有 /run/systemd/system 就认为是 systemd init，默认用 systemd
// driver；否则用 cgroupfs。接 Kubelet 时调用方应显式指定，不要依赖探测。
func autoDetectDriver() Driver {
	if fileExists("/run/systemd/system") {
		return DriverSystemd
	}
	return DriverCgroupfs
}
