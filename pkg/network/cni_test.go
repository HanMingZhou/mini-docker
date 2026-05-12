package network

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
)

func TestNewManagerNoConfig(t *testing.T) {
	// Non-existent conf dir → disabled manager, no error
	mgr, err := NewManager(t.TempDir(), t.TempDir(), []string{"/nonexistent"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mgr.Ready() {
		t.Errorf("manager should not be ready without config")
	}
	if mgr.NetworkName() != "" {
		t.Errorf("NetworkName = %q, want empty", mgr.NetworkName())
	}

	// Setup / Teardown must be no-ops when not ready
	ip, err := mgr.Setup(context.Background(), "pod1", "/proc/1/ns/net", "eth0", nil)
	if err != nil {
		t.Errorf("Setup when not ready returned err: %v", err)
	}
	if ip != "" {
		t.Errorf("Setup returned IP %q when not ready", ip)
	}
	if err := mgr.Teardown(context.Background(), "pod1", "/proc/1/ns/net", "eth0"); err != nil {
		t.Errorf("Teardown when not ready returned err: %v", err)
	}
}

func TestNewManagerLoadsConflist(t *testing.T) {
	confDir := t.TempDir()
	// Minimal valid conflist — plugin won't be invoked at load time.
	confPath := filepath.Join(confDir, "10-test.conflist")
	conflist := `{
        "cniVersion": "1.0.0",
        "name": "mini-docker-test",
        "plugins": [
            {
                "type": "bridge",
                "bridge": "mydocker0",
                "isGateway": true,
                "ipMasq": true,
                "ipam": {
                    "type": "host-local",
                    "subnet": "10.22.0.0/16"
                }
            }
        ]
    }`
	if err := os.WriteFile(confPath, []byte(conflist), 0644); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(confDir, t.TempDir(), []string{"/nonexistent"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mgr.Ready() {
		t.Fatalf("manager should be ready with valid conflist")
	}
	if mgr.NetworkName() != "mini-docker-test" {
		t.Errorf("NetworkName = %q, want mini-docker-test", mgr.NetworkName())
	}
}

func TestNewManagerFallsBackToConf(t *testing.T) {
	confDir := t.TempDir()
	// Single-plugin .conf (legacy format) — libcni should wrap it into a list.
	confPath := filepath.Join(confDir, "10-test.conf")
	conf := `{
        "cniVersion": "1.0.0",
        "name": "legacy-net",
        "type": "bridge",
        "ipam": { "type": "host-local", "subnet": "10.23.0.0/16" }
    }`
	if err := os.WriteFile(confPath, []byte(conf), 0644); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(confDir, t.TempDir(), []string{"/nonexistent"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mgr.Ready() {
		t.Fatalf("manager should be ready with legacy .conf")
	}
	if mgr.NetworkName() != "legacy-net" {
		t.Errorf("NetworkName = %q, want legacy-net", mgr.NetworkName())
	}
}

func TestNewManagerPrefersConflistAlphabetically(t *testing.T) {
	confDir := t.TempDir()
	// Two configs; manager should pick the lex-smallest.
	_ = os.WriteFile(filepath.Join(confDir, "20-second.conflist"), []byte(`{
        "cniVersion": "1.0.0", "name": "second",
        "plugins": [{"type":"bridge"}]
    }`), 0644)
	_ = os.WriteFile(filepath.Join(confDir, "10-first.conflist"), []byte(`{
        "cniVersion": "1.0.0", "name": "first",
        "plugins": [{"type":"bridge"}]
    }`), 0644)

	mgr, err := NewManager(confDir, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if mgr.NetworkName() != "first" {
		t.Errorf("NetworkName = %q, want first (alphabetical)", mgr.NetworkName())
	}
}

func TestExtractIPv4(t *testing.T) {
	// Build a fake CNI result with an IPv4 address.
	_, ipnet, _ := net.ParseCIDR("10.22.0.5/16")
	ipnet.IP = net.ParseIP("10.22.0.5")
	r := &current.Result{
		CNIVersion: "1.0.0",
		IPs: []*current.IPConfig{
			{Address: *ipnet},
		},
	}
	if got := extractIPv4(r); got != "10.22.0.5" {
		t.Errorf("extractIPv4 = %q, want 10.22.0.5", got)
	}
}

func TestExtractIPv4Nil(t *testing.T) {
	if got := extractIPv4(nil); got != "" {
		t.Errorf("nil result should return empty string, got %q", got)
	}
}

// Verify type assertions compile (we use types.Result in function signature).
var _ types.Result = (*current.Result)(nil)
