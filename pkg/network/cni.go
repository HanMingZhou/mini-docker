// Package network 实现容器网络管理。
//
// 采用 CNI（Container Network Interface）规范：
//   - 运行时负责创建 network namespace（由 pause 进程持有）。
//   - 运行时调用 CNI 插件二进制（/opt/cni/bin/*），通过 stdin 传 JSON 配置、
//     通过环境变量传命令和参数，从 stdout 读结果。
//   - 网络配置来自 /etc/cni/net.d/*.conflist。
//
// 本实现基于 containernetworking/cni 的 libcni 库，不重写插件调用协议。
package network

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
)

// debugEnabled 受环境变量 MYDOCKER_DEBUG 控制：非空 / "1" / "true" 时打印 debug 日志。
// 这样默认情况下 CLI 不会用日志噪声污染用户终端（和 docker 行为一致）。
var debugEnabled = func() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("MYDOCKER_DEBUG")))
	return v != "" && v != "0" && v != "false" && v != "no"
}()

// debugf prints to stderr only when MYDOCKER_DEBUG is set.
func debugf(format string, args ...any) {
	if debugEnabled {
		fmt.Fprintf(os.Stderr, format, args...)
	}
}

// DefaultConfDir 是 CNI 配置目录的默认位置。
const DefaultConfDir = "/etc/cni/net.d"

// DefaultBinDirs 是 CNI 插件二进制的搜索路径。
var DefaultBinDirs = []string{"/opt/cni/bin", "/usr/libexec/cni"}

// Manager 负责为 Pod 沙箱执行 CNI ADD / DEL。
type Manager struct {
	confDir  string
	binDirs  []string
	cni      libcni.CNI
	netList  *libcni.NetworkConfigList // 选中的默认网络（目前只用一个）
	cacheDir string
}

// NewManager 构造一个 CNI Manager。如果 confDir 下没有有效的 .conflist，
// 返回一个 "disabled" manager：Setup 是 no-op，Teardown 是 no-op，
// GetIP 总是返回空。这让 Kubelet 在缺 CNI 配置时也能握手成功（NetworkReady=false）。
func NewManager(confDir, cacheDir string, binDirs []string) (*Manager, error) {
	if confDir == "" {
		confDir = DefaultConfDir
	}
	if len(binDirs) == 0 {
		binDirs = DefaultBinDirs
	}
	if cacheDir == "" {
		cacheDir = "/var/lib/cni/cache"
	}

	m := &Manager{
		confDir:  confDir,
		binDirs:  binDirs,
		cacheDir: cacheDir,
	}
	m.cni = libcni.NewCNIConfigWithCacheDir(binDirs, cacheDir, nil)

	if err := m.loadDefaultNetwork(); err != nil {
		// 不致命：没网络插件也能跑（只是无法给 Pod 分 IP）。
		fmt.Fprintf(os.Stderr, "cni: no default network loaded: %v\n", err)
	}
	return m, nil
}

// Ready 表示 CNI 配置已加载且可用。
func (m *Manager) Ready() bool {
	return m.netList != nil
}

// loadDefaultNetwork 扫描 confDir，取第一个 .conflist（按文件名字母序）作为默认网络。
// 如果只有 .conf 而无 .conflist，也支持（用单插件构造成 list）。
func (m *Manager) loadDefaultNetwork() error {
	entries, err := os.ReadDir(m.confDir)
	if err != nil {
		return fmt.Errorf("read conf dir %s: %w", m.confDir, err)
	}
	var candidates []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".conflist") ||
			strings.HasSuffix(name, ".conf") ||
			strings.HasSuffix(name, ".json") {
			candidates = append(candidates, filepath.Join(m.confDir, name))
		}
	}
	if len(candidates) == 0 {
		return errors.New("no cni configs found")
	}
	sort.Strings(candidates)

	for _, p := range candidates {
		var list *libcni.NetworkConfigList
		if strings.HasSuffix(p, ".conflist") {
			list, err = libcni.ConfListFromFile(p)
		} else {
			var conf *libcni.NetworkConfig
			conf, err = libcni.ConfFromFile(p)
			if err == nil {
				list, err = libcni.ConfListFromConf(conf)
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "cni: skipping %s: %v\n", p, err)
			continue
		}
		m.netList = list
		debugf("cni: loaded network %q from %s\n", list.Name, p)
		return nil
	}
	return fmt.Errorf("no valid cni config in %s", m.confDir)
}

// PortMapping 描述一条端口映射（对应 CNI portmap 插件的格式）。
type PortMapping struct {
	HostPort      int32  `json:"hostPort"`
	ContainerPort int32  `json:"containerPort"`
	Protocol      string `json:"protocol,omitempty"` // "tcp" 或 "udp"，默认 tcp
	HostIP        string `json:"hostIP,omitempty"`
}

// Setup 为 Pod 沙箱执行 CNI ADD，返回分配的 IPv4 地址（可能为空）。
//
//	podID     — K8s pod UID 或沙箱 ID，作为 CNI ContainerID（需唯一）。
//	netnsPath — 形如 /proc/<pid>/ns/net。
//	ifName    — 容器内接口名，通常 "eth0"。
//
// 如果 CNI 未就绪（没有配置），返回空 IP 且 err == nil——调用方应只在 Ready()
// 为 true 时调用 Setup。
func (m *Manager) Setup(ctx context.Context, podID, netnsPath, ifName string, extraArgs [][2]string) (ip string, err error) {
	return m.SetupWithPorts(ctx, podID, netnsPath, ifName, extraArgs, nil)
}

// SetupWithPorts 执行 CNI ADD 并携带端口映射（走 portmap 插件）
func (m *Manager) SetupWithPorts(ctx context.Context, podID, netnsPath, ifName string, extraArgs [][2]string, ports []PortMapping) (ip string, err error) {
	if !m.Ready() {
		return "", nil
	}
	if ifName == "" {
		ifName = "eth0"
	}
	rt := &libcni.RuntimeConf{
		ContainerID: podID,
		NetNS:       netnsPath,
		IfName:      ifName,
		Args:        extraArgs,
	}
	if len(ports) > 0 {
		rt.CapabilityArgs = map[string]interface{}{
			"portMappings": ports,
		}
	}
	result, err := m.cni.AddNetworkList(ctx, m.netList, rt)
	if err != nil {
		return "", fmt.Errorf("cni ADD: %w", err)
	}
	return extractIPv4(result), nil
}

// Teardown 为 Pod 沙箱执行 CNI DEL。幂等：重复调用不报错。
func (m *Manager) Teardown(ctx context.Context, podID, netnsPath, ifName string) error {
	if !m.Ready() {
		return nil
	}
	if ifName == "" {
		ifName = "eth0"
	}
	rt := &libcni.RuntimeConf{
		ContainerID: podID,
		NetNS:       netnsPath,
		IfName:      ifName,
	}
	if err := m.cni.DelNetworkList(ctx, m.netList, rt); err != nil {
		// 对于 "netns 已不存在" 的情况，CNI 插件通常返回错误，但运行时仍应幂等。
		if strings.Contains(err.Error(), "no such file") || strings.Contains(err.Error(), "not exist") {
			return nil
		}
		return fmt.Errorf("cni DEL: %w", err)
	}
	return nil
}

// NetworkName 返回当前加载的 CNI 网络名（用于日志/调试），未就绪返回空。
func (m *Manager) NetworkName() string {
	if m.netList == nil {
		return ""
	}
	return m.netList.Name
}

// extractIPv4 从 CNI 结果中提取第一个 IPv4 地址（不含掩码）。
// 兼容 v0.3 / v0.4 / v1.0 spec——通过 result.GetAsVersion 转成 current。
func extractIPv4(r types.Result) string {
	if r == nil {
		return ""
	}
	res, err := current.NewResultFromResult(r)
	if err != nil {
		return ""
	}
	for _, ip := range res.IPs {
		if ip == nil {
			continue
		}
		// ip.Address.IP may be IPv4 or IPv6; filter for IPv4.
		if v4 := ip.Address.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}
