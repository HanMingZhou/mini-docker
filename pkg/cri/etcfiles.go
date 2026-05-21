package cri

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mini-docker/mini-docker/pkg/sandbox"
)

// writeContainerEtcFiles 在容器 rootfs (mergedRoot) 下生成 /etc/resolv.conf、
// /etc/hosts、/etc/hostname。
//
// 时机：CreateContainer 在 PrepareRootfs 之后、apply bind mounts 之前调用。
//
//   - resolv.conf 数据来自 sandbox 的 DNSConfig（Kubelet 通过
//     PodSandboxConfig.DnsConfig 传过来）。空时回退到宿主机 /etc/resolv.conf
//     的内容（让容器至少能解析外网，否则 CoreDNS 起不来）。
//   - hosts 至少包含 127.0.0.1 / ::1 项 + Pod 自己的 IP/hostname；
//     另外把 sandbox.HostAliases 写进去（如果有）。
//   - hostname 取 sandbox.Hostname，没有就回退到 sandbox 名字。
func writeContainerEtcFiles(mergedRoot string, sb *sandbox.Sandbox) error {
	if mergedRoot == "" {
		return fmt.Errorf("writeContainerEtcFiles: empty mergedRoot")
	}
	if sb == nil {
		return fmt.Errorf("writeContainerEtcFiles: nil sandbox")
	}

	etc := filepath.Join(mergedRoot, "etc")
	if err := os.MkdirAll(etc, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", etc, err)
	}

	// 1) /etc/resolv.conf
	if err := writeResolvConf(etc, sb.DNS); err != nil {
		return fmt.Errorf("write resolv.conf: %w", err)
	}

	// 2) /etc/hosts
	if err := writeHosts(etc, sb); err != nil {
		return fmt.Errorf("write hosts: %w", err)
	}

	// 3) /etc/hostname
	hn := sb.Hostname
	if hn == "" {
		hn = sb.Metadata.Name
	}
	if err := os.WriteFile(filepath.Join(etc, "hostname"), []byte(hn+"\n"), 0644); err != nil {
		return fmt.Errorf("write hostname: %w", err)
	}
	return nil
}

// resolvConfContent 把 DNSConfig 序列化成 resolv.conf 的内容。
// 空 DNS 时回退到宿主机 /etc/resolv.conf。
func resolvConfContent(dns sandbox.DNSConfig) []byte {
	if len(dns.Servers) == 0 && len(dns.Searches) == 0 && len(dns.Options) == 0 {
		// 容器没收到任何 DNS 配置（独立 mydocker run 或 Kubelet 未传）：
		// 复制宿主机配置作为兜底，让容器能解析外网。
		if data, err := os.ReadFile("/etc/resolv.conf"); err == nil {
			return data
		}
		// 宿主也读不到——再不济也写 8.8.8.8，防止 forward 插件失败。
		return []byte("nameserver 8.8.8.8\n")
	}

	var b strings.Builder
	for _, s := range dns.Servers {
		fmt.Fprintf(&b, "nameserver %s\n", s)
	}
	if len(dns.Searches) > 0 {
		fmt.Fprintf(&b, "search %s\n", strings.Join(dns.Searches, " "))
	}
	if len(dns.Options) > 0 {
		fmt.Fprintf(&b, "options %s\n", strings.Join(dns.Options, " "))
	}
	return []byte(b.String())
}

func writeResolvConf(etcDir string, dns sandbox.DNSConfig) error {
	return os.WriteFile(filepath.Join(etcDir, "resolv.conf"), resolvConfContent(dns), 0644)
}

// hostsContent 拼出 /etc/hosts 文件内容。
func hostsContent(sb *sandbox.Sandbox) []byte {
	var b strings.Builder
	// 标准 loopback 项（docker / containerd 也写这些）
	b.WriteString("127.0.0.1\tlocalhost\n")
	b.WriteString("::1\tlocalhost ip6-localhost ip6-loopback\n")
	b.WriteString("fe00::0\tip6-localnet\n")
	b.WriteString("ff00::0\tip6-mcastprefix\n")
	b.WriteString("ff02::1\tip6-allnodes\n")
	b.WriteString("ff02::2\tip6-allrouters\n")

	// Pod 自己的 IP -> hostname
	hn := sb.Hostname
	if hn == "" {
		hn = sb.Metadata.Name
	}
	if sb.IP != "" && hn != "" {
		fmt.Fprintf(&b, "%s\t%s\n", sb.IP, hn)
	}

	// 用户自定义 hostAliases
	for _, a := range sb.HostAliases {
		if a.IP == "" || len(a.Hostnames) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s\t%s\n", a.IP, strings.Join(a.Hostnames, " "))
	}
	return []byte(b.String())
}

func writeHosts(etcDir string, sb *sandbox.Sandbox) error {
	return os.WriteFile(filepath.Join(etcDir, "hosts"), hostsContent(sb), 0644)
}
