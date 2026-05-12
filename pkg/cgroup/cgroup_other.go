//go:build !linux

package cgroup

import "errors"

func newManager(_ Config) (Manager, error) {
	return nil, errors.New("cgroup only works on Linux")
}

// fileExists stub for autoDetectDriver on non-Linux.
func fileExists(_ string) bool { return false }
