//go:build linux

package cgroup

import (
	"testing"

	systemdDbus "github.com/coreos/go-systemd/v22/dbus"
)

func TestLastSlice(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"kubepods.slice", "kubepods.slice"},
		{"kubepods.slice/kubepods-besteffort.slice", "kubepods-besteffort.slice"},
		{"a/b/c", "c"},
		{"trailing/", "trailing"},
	}
	for _, tc := range tests {
		if got := lastSlice(tc.in); got != tc.want {
			t.Errorf("lastSlice(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeUnitName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"abc", "abc"},
		{"a/b", "a_b"},
		{"abc:xyz", "abc-xyz"},
		{"k8s/pod:1", "k8s_pod-1"},
	}
	for _, tc := range tests {
		if got := sanitizeUnitName(tc.in); got != tc.want {
			t.Errorf("sanitizeUnitName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResourcesToProperties(t *testing.T) {
	// No limits -> no properties
	if p := resourcesToProperties(Resources{}); len(p) != 0 {
		t.Errorf("empty resources should produce no properties, got %d", len(p))
	}

	// Memory only
	props := resourcesToProperties(Resources{MemoryBytes: 100 << 20})
	if len(props) != 1 || props[0].Name != "MemoryMax" {
		t.Errorf("expected single MemoryMax, got %+v", props)
	}

	// CPU: 50000 quota over 100000 period = 500000 us/s (half a core)
	props = resourcesToProperties(Resources{CPUQuotaMicros: 50000, CPUPeriodMicros: 100000})
	if len(props) != 1 {
		t.Fatalf("expected 1 property, got %d", len(props))
	}
	if props[0].Name != "CPUQuotaPerSecUSec" {
		t.Errorf("expected CPUQuotaPerSecUSec, got %q", props[0].Name)
	}

	// Period defaulting
	props = resourcesToProperties(Resources{CPUQuotaMicros: 200000})
	if len(props) != 1 {
		t.Fatalf("expected 1 property with default period, got %d", len(props))
	}

	// Both memory and cpu
	props = resourcesToProperties(Resources{
		MemoryBytes:     512 << 20,
		CPUQuotaMicros:  100000,
		CPUPeriodMicros: 100000,
	})
	if len(props) != 2 {
		t.Errorf("expected 2 properties, got %d: %+v", len(props), props)
	}
}

// Ensure we import systemdDbus correctly.
var _ systemdDbus.Property
