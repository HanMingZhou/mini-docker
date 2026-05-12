package cgroup

import (
	"testing"
)

func TestNewWithConfigValidation(t *testing.T) {
	if _, err := NewWithConfig(Config{Name: "", Driver: DriverCgroupfs}); err == nil {
		t.Errorf("expected error for empty name")
	}
}

func TestAutoDetectDriver(t *testing.T) {
	// Just make sure it returns a valid driver string.
	d := autoDetectDriver()
	if d != DriverCgroupfs && d != DriverSystemd {
		t.Errorf("unexpected driver: %q", d)
	}
}
