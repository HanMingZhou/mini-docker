// Package sandbox 管理 Pod 沙箱。
//
// 沙箱 = 一个常驻的 pause 进程 + 独立的 network/UTS/IPC namespace。
// 业务容器通过 setns 加入这些 namespace，实现"同 Pod 容器共享网络/IPC"。
//
// 目录结构：
//
//	<root>/sandboxes/<id>/config.json
//
// pause 进程本身用 `/proc/self/exe pause` 启动（复用 mydocker 二进制）。
package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// State 是沙箱的生命周期状态。
// 对齐 CRI 的 PodSandboxState 枚举语义：
//
//	SANDBOX_READY    - 运行中
//	SANDBOX_NOTREADY - 已停止
type State string

const (
	StateReady    State = "Ready"
	StateNotReady State = "NotReady"
)

// Metadata 描述 Pod 沙箱的身份信息（与 CRI PodSandboxMetadata 对齐）。
type Metadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	UID       string `json:"uid"`
	Attempt   uint32 `json:"attempt"`
}

// HostAlias maps an IP address to one or more hostnames; written into
// /etc/hosts of every container in the sandbox.
type HostAlias struct {
	IP        string   `json:"ip"`
	Hostnames []string `json:"hostnames"`
}

// DNSConfig captures the resolv.conf-relevant fields the sandbox should
// hand down to its containers (mirrors CRI DNSConfig).
type DNSConfig struct {
	Servers  []string `json:"servers,omitempty"`
	Searches []string `json:"searches,omitempty"`
	Options  []string `json:"options,omitempty"`
}

// Sandbox 是一个沙箱的完整持久化元数据。
type Sandbox struct {
	ID           string            `json:"id"`
	Metadata     Metadata          `json:"metadata"`
	State        State             `json:"state"`
	PID          int               `json:"pid"`          // pause 进程的宿主机 PID
	NetnsPath    string            `json:"netns_path"`   // /proc/<pid>/ns/net，便于业务容器 setns
	IP           string            `json:"ip,omitempty"` // CNI 分配的 IPv4，无则空
	CgroupParent string            `json:"cgroup_parent,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	FinishedAt   time.Time         `json:"finished_at,omitempty"`
	LogDir       string            `json:"log_directory,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`

	// /etc/* generation inputs (used by CRI when preparing per-container rootfs).
	Hostname    string      `json:"hostname,omitempty"`
	DNS         DNSConfig   `json:"dns,omitempty"`
	HostAliases []HostAlias `json:"host_aliases,omitempty"`
}

// Manager 是沙箱的对外入口。
type Manager struct {
	root    string // 数据根目录（通常 = store.Root()）
	selfExe string // mydocker 可执行文件路径，用作 pause 载体；为空时用 /proc/self/exe
	netHook NetworkHook
}

// NetworkHook 允许 Manager 在沙箱启动/停止时调用外部网络子系统（CNI）。
// 保持接口让 sandbox 包不依赖 network 包。
type NetworkHook interface {
	// Setup 在 pause 进程起来之后、save 之前调用。返回 IPv4（可空）。
	Setup(podID, netnsPath string) (ip string, err error)
	// Teardown 在 Stop 之前调用。幂等。
	Teardown(podID, netnsPath string) error
}

// SetNetworkHook 配置网络钩子；空则不做任何网络操作。
func (m *Manager) SetNetworkHook(h NetworkHook) {
	m.netHook = h
}

// NewManager 创建 Manager。selfExe 如果为空，会自动用 /proc/self/exe。
func NewManager(root, selfExe string) (*Manager, error) {
	if root == "" {
		return nil, errors.New("sandbox manager: empty root")
	}
	sd := filepath.Join(root, "sandboxes")
	if err := os.MkdirAll(sd, 0755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", sd, err)
	}
	if selfExe == "" {
		selfExe = "/proc/self/exe"
	}
	return &Manager{root: root, selfExe: selfExe}, nil
}

// SandboxDir 返回沙箱元数据目录（可能未创建）。
func (m *Manager) SandboxDir(id string) string {
	return filepath.Join(m.root, "sandboxes", id)
}

// RootPath 返回 Manager 的数据根目录。
func (m *Manager) RootPath() string {
	return m.root
}

// StartOptions 是 Start 的可选参数，避免签名膨胀。
type StartOptions struct {
	LogDir       string
	Labels       map[string]string
	Annotations  map[string]string
	CgroupParent string
	HostNetwork  bool // true = 不创建新 netns，共享宿主机网络（hostNetwork Pod）

	// /etc/* 生成所需输入；为空时使用合理默认（hostname=metadata.name，
	// DNS 留空让 CRI 层兜底）。
	Hostname    string
	DNS         DNSConfig
	HostAliases []HostAlias
}

// Start 创建并启动一个新沙箱。
// 成功后 pause 进程已在独立 netns/uts/ipc 中运行。
//
// 向后兼容的简易入口，建议使用 StartWithOptions。
func (m *Manager) Start(md Metadata, logDir string, labels, annotations map[string]string) (*Sandbox, error) {
	return m.start(md, StartOptions{
		LogDir:      logDir,
		Labels:      labels,
		Annotations: annotations,
	})
}

// StartWithOptions 使用选项结构创建沙箱。
func (m *Manager) StartWithOptions(md Metadata, opts StartOptions) (*Sandbox, error) {
	return m.start(md, opts)
}

// Stop 停止沙箱的 pause 进程（netns 随进程消亡）。幂等。
// 如果沙箱记录已不存在（被 Remove 或手动清理），视为已停止，不报错。
func (m *Manager) Stop(id string) error {
	err := m.stop(id)
	if err != nil && os.IsNotExist(err) {
		return nil // 幂等：记录已不在
	}
	return err
}

// Remove 删除沙箱记录与目录。要求沙箱已停止。
// 如果沙箱已不存在，视为幂等成功。
func (m *Manager) Remove(id string) error {
	sb, err := m.Get(id)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 幂等
		}
		return err
	}
	if sb.State == StateReady && pidAlive(sb.PID) {
		return fmt.Errorf("sandbox %s is still ready; stop it first", id)
	}
	return os.RemoveAll(m.SandboxDir(id))
}

// Get 根据 ID 读取沙箱；同时刷新状态（pause 死了 -> NotReady）。
// 如果 config.json 不存在，返回 os.ErrNotExist 包装的错误。
func (m *Manager) Get(id string) (*Sandbox, error) {
	sb, err := m.loadByID(id)
	if err != nil {
		return nil, err
	}
	m.refreshState(sb)
	return sb, nil
}

// Resolve 支持完整 ID / ID 前缀 / Name 查找。
func (m *Manager) Resolve(ref string) (*Sandbox, error) {
	if ref == "" {
		return nil, errors.New("empty sandbox reference")
	}
	all, err := m.List()
	if err != nil {
		return nil, err
	}
	var matches []*Sandbox
	for _, s := range all {
		if s.ID == ref || s.Metadata.Name == ref {
			return s, nil
		}
		if strings.HasPrefix(s.ID, ref) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no such sandbox: %s", ref)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("ambiguous sandbox reference %q matches %d sandboxes", ref, len(matches))
	}
}

// List 列出所有沙箱。读取时会刷新状态。
func (m *Manager) List() ([]*Sandbox, error) {
	dir := filepath.Join(m.root, "sandboxes")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]*Sandbox, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sb, err := m.loadByID(e.Name())
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: load sandbox %s: %v\n", e.Name(), err)
			continue
		}
		m.refreshState(sb)
		out = append(out, sb)
	}
	// 新的在前
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// save 把 Sandbox 原子写入磁盘。
func (m *Manager) save(sb *Sandbox) error {
	dir := m.SandboxDir(sb.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sb, "", "  ")
	if err != nil {
		return err
	}
	target := filepath.Join(dir, "config.json")
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

func (m *Manager) loadByID(id string) (*Sandbox, error) {
	p := filepath.Join(m.SandboxDir(id), "config.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var sb Sandbox
	if err := json.Unmarshal(data, &sb); err != nil {
		return nil, fmt.Errorf("decode %s: %w", p, err)
	}
	return &sb, nil
}

// refreshState 根据 pause 进程是否存活，刷新并持久化状态。
// PID 1 的 sandbox（hostNetwork）永远 Ready。
func (m *Manager) refreshState(sb *Sandbox) {
	if sb.PID == 1 {
		return // PID 1 永远活着
	}
	if sb.State == StateReady && !pidAlive(sb.PID) {
		sb.State = StateNotReady
		sb.FinishedAt = time.Now().UTC()
		_ = m.save(sb)
	}
}
