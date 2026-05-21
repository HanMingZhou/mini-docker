package cri

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mini-docker/mini-docker/pkg/sandbox"
)

func TestResolvConfContent_FromDNSConfig(t *testing.T) {
	got := string(resolvConfContent(sandbox.DNSConfig{
		Servers:  []string{"10.96.0.10", "8.8.8.8"},
		Searches: []string{"default.svc.cluster.local", "svc.cluster.local", "cluster.local"},
		Options:  []string{"ndots:5"},
	}))
	want := strings.Join([]string{
		"nameserver 10.96.0.10",
		"nameserver 8.8.8.8",
		"search default.svc.cluster.local svc.cluster.local cluster.local",
		"options ndots:5",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("resolv.conf mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestResolvConfContent_EmptyFallsBack(t *testing.T) {
	// 空 DNSConfig 时回退到宿主或常量；至少要包含一行 nameserver。
	got := string(resolvConfContent(sandbox.DNSConfig{}))
	if !strings.Contains(got, "nameserver") {
		t.Fatalf("expected fallback resolv.conf with at least one nameserver, got: %q", got)
	}
}

func TestHostsContent_BasicLoopbackAndPodIP(t *testing.T) {
	sb := &sandbox.Sandbox{
		Metadata: sandbox.Metadata{Name: "demo-pod"},
		Hostname: "demo-pod",
		IP:       "10.244.0.5",
	}
	got := string(hostsContent(sb))
	for _, sub := range []string{
		"127.0.0.1\tlocalhost\n",
		"::1\tlocalhost",
		"10.244.0.5\tdemo-pod\n",
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("hosts missing %q\n----\n%s", sub, got)
		}
	}
}

func TestHostsContent_HostAliases(t *testing.T) {
	sb := &sandbox.Sandbox{
		Metadata: sandbox.Metadata{Name: "p"},
		HostAliases: []sandbox.HostAlias{
			{IP: "1.2.3.4", Hostnames: []string{"foo.example", "bar"}},
		},
	}
	got := string(hostsContent(sb))
	if !strings.Contains(got, "1.2.3.4\tfoo.example bar\n") {
		t.Errorf("hostAlias not rendered:\n%s", got)
	}
}

func TestWriteContainerEtcFiles_Writes3Files(t *testing.T) {
	merged := t.TempDir()
	sb := &sandbox.Sandbox{
		Metadata: sandbox.Metadata{Name: "demo"},
		Hostname: "demo",
		IP:       "10.244.0.5",
		DNS: sandbox.DNSConfig{
			Servers:  []string{"10.96.0.10"},
			Searches: []string{"cluster.local"},
		},
	}
	if err := writeContainerEtcFiles(merged, sb); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, f := range []string{"resolv.conf", "hosts", "hostname"} {
		p := filepath.Join(merged, "etc", f)
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
		if st.Size() == 0 {
			t.Errorf("%s is empty", f)
		}
	}

	// hostname must end with newline (POSIX convention)
	hn, _ := os.ReadFile(filepath.Join(merged, "etc/hostname"))
	if string(hn) != "demo\n" {
		t.Errorf("hostname=%q, want \"demo\\n\"", string(hn))
	}
}
