//go:build linux

package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// cgroup v2 的统一挂载点。
const cgroupV2Root = "/sys/fs/cgroup"

func newManager(cfg Config) (Manager, error) {
	if !isV2() {
		return nil, errors.New("cgroups v2 not available; v1 support not implemented yet")
	}
	switch cfg.Driver {
	case DriverCgroupfs:
		return newCgroupfsManager(cfg)
	case DriverSystemd:
		return newSystemdManager(cfg)
	default:
		return nil, fmt.Errorf("unknown cgroup driver: %q", cfg.Driver)
	}
}

// --- cgroupfs driver --------------------------------------------------------

// v2Manager 是 cgroups v2 直接操作文件系统的实现。
type v2Manager struct {
	name string // 叶子 cgroup 名
	path string // 绝对路径
}

func newCgroupfsManager(cfg Config) (Manager, error) {
	parent := strings.TrimPrefix(cfg.Parent, "/")
	path := filepath.Join(cgroupV2Root, parent, cfg.Name)
	return &v2Manager{name: cfg.Name, path: path}, nil
}

// isV2 检查 /sys/fs/cgroup 是否为 cgroup2fs。
func isV2() bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(cgroupV2Root, &st); err != nil {
		return false
	}
	// magic 常量来自 <linux/magic.h>：CGROUP2_SUPER_MAGIC = 0x63677270
	const cgroup2Magic = 0x63677270
	return st.Type == cgroup2Magic
}

func (m *v2Manager) Path() string { return m.path }

func (m *v2Manager) Apply(r Resources) error {
	// 父级必须在 cgroup.subtree_control 里显式 enable 子 cgroup 要用的控制器。
	// 这里只确保 memory 和 cpu 开启，没启用就尝试启用（需要 root）。
	if err := ensureControllerAt(filepath.Dir(m.path), "memory"); err != nil {
		return err
	}
	if err := ensureControllerAt(filepath.Dir(m.path), "cpu"); err != nil {
		return err
	}

	if err := os.MkdirAll(m.path, 0755); err != nil {
		return fmt.Errorf("mkdir cgroup %s: %w", m.path, err)
	}

	if r.MemoryBytes > 0 {
		if err := writeFile(filepath.Join(m.path, "memory.max"),
			strconv.FormatInt(r.MemoryBytes, 10)); err != nil {
			return err
		}
	}

	if r.CPUQuotaMicros > 0 {
		period := r.CPUPeriodMicros
		if period <= 0 {
			period = 100000 // 默认 100ms
		}
		val := fmt.Sprintf("%d %d", r.CPUQuotaMicros, period)
		if err := writeFile(filepath.Join(m.path, "cpu.max"), val); err != nil {
			return err
		}
	}
	return nil
}

func (m *v2Manager) AddProc(pid int) error {
	return writeFile(filepath.Join(m.path, "cgroup.procs"), strconv.Itoa(pid))
}

func (m *v2Manager) Destroy() error {
	if err := os.Remove(m.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove cgroup %s: %w", m.path, err)
	}
	return nil
}

func (m *v2Manager) Freeze(freeze bool) error {
	val := "0"
	if freeze {
		val = "1"
	}
	return writeFile(filepath.Join(m.path, "cgroup.freeze"), val)
}

// ensureControllerAt 保证指定 cgroup 目录的 subtree_control 启用了 name 控制器。
// 对所有祖先目录递归启用（v2 要求每一层都 enable 才能往下传）。
func ensureControllerAt(dir, name string) error {
	// 逐级从 cgroupV2Root 到 dir，一层层 enable
	rel, err := filepath.Rel(cgroupV2Root, dir)
	if err != nil || rel == "." || rel == "" {
		return ensureControllerAtFile(cgroupV2Root, name)
	}
	current := cgroupV2Root
	for _, seg := range strings.Split(rel, string(os.PathSeparator)) {
		if err := ensureControllerAtFile(current, name); err != nil {
			return err
		}
		current = filepath.Join(current, seg)
		// 祖先目录必须存在才能继续；不存在就 mkdir
		if err := os.MkdirAll(current, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", current, err)
		}
	}
	return nil
}

func ensureControllerAtFile(dir, name string) error {
	ctrlFile := filepath.Join(dir, "cgroup.subtree_control")
	data, err := os.ReadFile(ctrlFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 顶层都没这个文件的话，内核没 v2，上层会失败
		}
		return fmt.Errorf("read %s: %w", ctrlFile, err)
	}
	for _, c := range strings.Fields(string(data)) {
		if c == name {
			return nil
		}
	}
	return writeFile(ctrlFile, "+"+name)
}

func writeFile(path, value string) error {
	if err := os.WriteFile(path, []byte(value), 0644); err != nil {
		return fmt.Errorf("write %s = %q: %w", path, value, err)
	}
	return nil
}

// fileExists is used by autoDetectDriver.
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
