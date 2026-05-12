// portmap.go 实现容器端口映射（-p hostPort:containerPort）。
//
// 通过 iptables 命令行工具添加/删除 DNAT 规则。
// 仅支持 Linux；非 Linux 平台编译时由 portmap_other.go 提供空实现。
//
// 规则链：
//   - PREROUTING (nat): 外部流量 → 容器
//   - OUTPUT (nat): 本机流量 → 容器（localhost 访问）
//
// Docker 用一个自定义链 DOCKER，我们简化为直接插入标准链，
// 用 comment 标记便于精确删除。

package network

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// PortMapping is defined in cni.go — reuse it here.
// (Shared type: HostPort int32, ContainerPort int32, Protocol string, HostIP string)

// ParsePortMappings 解析 `-p` 参数列表。
// 格式：hostPort:containerPort[/proto]
// 例如：8080:80, 53:53/udp, 443:443/tcp
func ParsePortMappings(raw []string) ([]PortMapping, error) {
	out := make([]PortMapping, 0, len(raw))
	for _, s := range raw {
		pm, err := parseOnePort(s)
		if err != nil {
			return nil, fmt.Errorf("-p %q: %w", s, err)
		}
		out = append(out, pm)
	}
	return out, nil
}

func parseOnePort(s string) (PortMapping, error) {
	proto := "tcp"
	// Split off /proto suffix
	if idx := strings.LastIndex(s, "/"); idx > 0 {
		proto = strings.ToLower(s[idx+1:])
		s = s[:idx]
		if proto != "tcp" && proto != "udp" {
			return PortMapping{}, fmt.Errorf("unsupported protocol %q", proto)
		}
	}
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return PortMapping{}, fmt.Errorf("want hostPort:containerPort")
	}
	hp, err := strconv.Atoi(parts[0])
	if err != nil || hp <= 0 || hp > 65535 {
		return PortMapping{}, fmt.Errorf("invalid host port %q", parts[0])
	}
	cp, err := strconv.Atoi(parts[1])
	if err != nil || cp <= 0 || cp > 65535 {
		return PortMapping{}, fmt.Errorf("invalid container port %q", parts[1])
	}
	return PortMapping{HostPort: int32(hp), ContainerPort: int32(cp), Protocol: proto}, nil
}

// AddPortMappings 为容器添加 iptables DNAT 规则。
// containerIP 是 CNI 分配的 IPv4（如 "10.33.0.5"）。
// comment 用于标记规则，方便后续精确删除（通常用容器 ID）。
func AddPortMappings(containerIP, comment string, mappings []PortMapping) error {
	for _, pm := range mappings {
		proto := pm.Protocol
		if proto == "" {
			proto = "tcp"
		}
		dst := fmt.Sprintf("%s:%d", containerIP, pm.ContainerPort)
		// PREROUTING: 外部流量
		if err := iptables("-t", "nat", "-A", "PREROUTING",
			"-p", proto, "--dport", strconv.Itoa(int(pm.HostPort)),
			"-j", "DNAT", "--to-destination", dst,
			"-m", "comment", "--comment", comment); err != nil {
			return fmt.Errorf("PREROUTING %d→%s: %w", pm.HostPort, dst, err)
		}
		// OUTPUT: 本机 localhost 流量
		if err := iptables("-t", "nat", "-A", "OUTPUT",
			"-p", proto, "--dport", strconv.Itoa(int(pm.HostPort)),
			"-j", "DNAT", "--to-destination", dst,
			"-m", "comment", "--comment", comment); err != nil {
			return fmt.Errorf("OUTPUT %d→%s: %w", pm.HostPort, dst, err)
		}
		// POSTROUTING MASQUERADE: 让容器看到的源 IP 是网关而不是 127.0.0.1，
		// 否则容器回包路由不通（hairpin NAT 问题）。
		if err := iptables("-t", "nat", "-A", "POSTROUTING",
			"-p", proto, "-d", containerIP, "--dport", strconv.Itoa(int(pm.ContainerPort)),
			"-j", "MASQUERADE",
			"-m", "comment", "--comment", comment); err != nil {
			return fmt.Errorf("POSTROUTING MASQUERADE: %w", err)
		}
	}
	return nil
}

// RemovePortMappings 删除之前添加的 DNAT 规则。幂等：规则不存在不报错。
func RemovePortMappings(containerIP, comment string, mappings []PortMapping) {
	for _, pm := range mappings {
		proto := pm.Protocol
		if proto == "" {
			proto = "tcp"
		}
		dst := fmt.Sprintf("%s:%d", containerIP, pm.ContainerPort)
		_ = iptables("-t", "nat", "-D", "PREROUTING",
			"-p", proto, "--dport", strconv.Itoa(int(pm.HostPort)),
			"-j", "DNAT", "--to-destination", dst,
			"-m", "comment", "--comment", comment)
		_ = iptables("-t", "nat", "-D", "OUTPUT",
			"-p", proto, "--dport", strconv.Itoa(int(pm.HostPort)),
			"-j", "DNAT", "--to-destination", dst,
			"-m", "comment", "--comment", comment)
		_ = iptables("-t", "nat", "-D", "POSTROUTING",
			"-p", proto, "-d", containerIP, "--dport", strconv.Itoa(int(pm.ContainerPort)),
			"-j", "MASQUERADE",
			"-m", "comment", "--comment", comment)
	}
}

func iptables(args ...string) error {
	cmd := exec.Command("iptables", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}
