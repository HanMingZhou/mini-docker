//go:build linux

package cgroup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	systemdDbus "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

// systemdManager 是通过 systemd 管理 cgroup 的实现。
//
// 流程：
//  1. Apply(r) 只把 Resources 暂存起来（systemd 没法"先建 scope 再 exec"，
//     我们按与 runc 一致的套路：在 AddProc 时真正 StartTransientUnit）。
//  2. AddProc(pid) 调 dbus 的 StartTransientUnit，把 pid 包进 <name>.scope，
//     同时把资源限制作为 unit 属性下发。systemd 随后创建 cgroup 目录。
//  3. Destroy() 调 StopUnit 让 systemd 清理 scope；scope 销毁时 cgroup 也一起走。
//
// Parent 的语义：是 systemd slice 名（点号分隔，例如
// "kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod<UID>.slice"）。
// Kubelet 会在 CRI 请求里通过 `cgroup_parent` 传进来。
type systemdManager struct {
	name      string // 叶子 scope 名，不含 ".scope" 后缀
	parent    string // slice 路径，斜杠分隔（K8s 传进来就是这种形式）
	resources Resources
	started   bool // StartTransientUnit 已调用？（用于 Destroy 判断）
}

func newSystemdManager(cfg Config) (Manager, error) {
	name := sanitizeUnitName(cfg.Name)
	parent := cfg.Parent
	if parent == "" {
		// systemd 默认把没指定 Slice 的 transient scope 放到 system.slice 下。
		// 我们显式跟上，这样 Path() 返回的 cgroup 路径与 systemd 实际创建的一致。
		parent = "system.slice"
	}
	return &systemdManager{
		name:   name,
		parent: parent,
	}, nil
}

func (m *systemdManager) Path() string {
	// systemd 在 cgroup v2 下的路径：
	//   /sys/fs/cgroup/<parent-expanded>/<name>.scope
	// parent 形如 "kubepods.slice/kubepods-besteffort.slice/..."；
	// v2 统一层级下每段都是真实目录，直接拼即可。
	return filepath.Join(cgroupV2Root, m.parent, m.unitName())
}

func (m *systemdManager) unitName() string {
	return m.name + ".scope"
}

// Apply 对 systemd driver 来说是 noop（真正的下发推迟到 AddProc）。
// 这里只记录资源限制。
func (m *systemdManager) Apply(r Resources) error {
	m.resources = r
	return nil
}

// AddProc 创建 transient scope 把 pid 装进去，同时下发资源限制。
func (m *systemdManager) AddProc(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid: %d", pid)
	}
	if m.started {
		// 已经起过 scope 了：把 pid 直接写到 cgroup.procs（共享 scope 的罕见路径）
		return writeFile(filepath.Join(m.Path(), "cgroup.procs"), fmt.Sprint(pid))
	}

	conn, err := systemdDbus.NewWithContext(context.Background())
	if err != nil {
		return fmt.Errorf("connect systemd dbus: %w", err)
	}
	defer conn.Close()

	properties := []systemdDbus.Property{
		systemdDbus.PropDescription(fmt.Sprintf("mydocker container %s", m.name)),
		{Name: "PIDs", Value: dbus.MakeVariant([]uint32{uint32(pid)})},
		// Delegate=yes 让我们（容器 init）后续可以在 scope 内创建子 cgroup
		{Name: "Delegate", Value: dbus.MakeVariant(true)},
	}
	if m.parent != "" {
		properties = append(properties,
			systemdDbus.PropSlice(lastSlice(m.parent)))
	}
	properties = append(properties, resourcesToProperties(m.resources)...)

	// 单次 RPC：创建 scope，返回一个 job 对象；等 done 通道收到 "done"
	ch := make(chan string, 1)
	if _, err := conn.StartTransientUnitContext(context.Background(),
		m.unitName(), "replace", properties, ch); err != nil {
		return fmt.Errorf("StartTransientUnit %s: %w", m.unitName(), err)
	}
	select {
	case result := <-ch:
		if result != "done" {
			return fmt.Errorf("StartTransientUnit %s returned %q", m.unitName(), result)
		}
	case <-time.After(10 * time.Second):
		return fmt.Errorf("StartTransientUnit %s: timeout", m.unitName())
	}
	m.started = true
	return nil
}

func (m *systemdManager) Destroy() error {
	if !m.started {
		return nil
	}
	conn, err := systemdDbus.NewWithContext(context.Background())
	if err != nil {
		return fmt.Errorf("connect systemd dbus: %w", err)
	}
	defer conn.Close()

	ch := make(chan string, 1)
	if _, err := conn.StopUnitContext(context.Background(), m.unitName(), "replace", ch); err != nil {
		// "not loaded" / "no such unit" 视为已停
		if isUnitNotFound(err) {
			return nil
		}
		return fmt.Errorf("StopUnit %s: %w", m.unitName(), err)
	}
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		// 不阻塞太久
	}
	return nil
}

// Freeze 直接写 cgroup.freeze。systemd 的 FreezeUnit D-Bus 方法也可，但我们
// 已经知道 scope 路径，写文件更直接、不用新 D-Bus 调用。
func (m *systemdManager) Freeze(freeze bool) error {
	val := "0"
	if freeze {
		val = "1"
	}
	return writeFile(filepath.Join(m.Path(), "cgroup.freeze"), val)
}

func isUnitNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Unit not loaded") ||
		strings.Contains(msg, "not loaded") ||
		strings.Contains(msg, "No such file")
}

// resourcesToProperties 把 Resources 转换成 systemd unit 属性。
//
// 关键映射：
//   - MemoryMax (bytes)  <=  MemoryBytes
//   - CPUQuotaPerSecUSec (microseconds per second) <= CPUQuotaMicros/Period * 1_000_000
//     systemd 的 CPUQuota 是 "每秒多少微秒"，不是 cgroup v2 的 period-based quota。
//     换算：systemd_us = quota_us * (1s / period_us) = quota_us * 1e6 / period_us
func resourcesToProperties(r Resources) []systemdDbus.Property {
	var props []systemdDbus.Property
	if r.MemoryBytes > 0 {
		props = append(props, systemdDbus.Property{
			Name:  "MemoryMax",
			Value: dbus.MakeVariant(uint64(r.MemoryBytes)),
		})
	}
	if r.CPUQuotaMicros > 0 {
		period := r.CPUPeriodMicros
		if period <= 0 {
			period = 100000
		}
		// systemd wants CPUQuotaPerSecUSec (microseconds per real second)
		quotaPerSec := uint64(r.CPUQuotaMicros) * 1_000_000 / uint64(period)
		props = append(props, systemdDbus.Property{
			Name:  "CPUQuotaPerSecUSec",
			Value: dbus.MakeVariant(quotaPerSec),
		})
	}
	return props
}

// lastSlice 取 parent 路径的最后一段作为 PropSlice 的值。
// 例如 "kubepods.slice/kubepods-besteffort.slice/pod-xxx.slice"
// 返回 "pod-xxx.slice"。
func lastSlice(parent string) string {
	parts := strings.Split(strings.TrimSuffix(parent, "/"), "/")
	return parts[len(parts)-1]
}

// sanitizeUnitName 把任意字符串变成合法的 systemd unit 名（去掉斜杠）。
func sanitizeUnitName(name string) string {
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, ":", "-")
	return name
}

// Silence the linter for an unused import warning when the build succeeds.
var _ = errors.New
