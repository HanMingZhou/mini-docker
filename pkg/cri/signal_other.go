//go:build !linux

package cri

import "errors"

// Non-Linux stubs — mydocker-cri only runs on Linux, but these let tests
// and code editing work on Darwin/Windows without build errors.
func pidAlive(_ int) bool  { return false }
func sendTerm(_ int) error { return errors.New("sendTerm only works on Linux") }
func sendKill(_ int) error { return errors.New("sendKill only works on Linux") }
func bindMountIntoRootfs(_, _, _ string, _ bool) error {
	return errors.New("bindMount only works on Linux")
}
