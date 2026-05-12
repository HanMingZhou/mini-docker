//go:build !linux

package main

import "errors"

// 非 Linux 平台这些都是占位实现，mydocker 的运行环境只支持 Linux。
func pidAlive(_ int) bool { return false }
func sendTerm(_ int) error {
	return errors.New("sendTerm only works on Linux")
}
func sendKill(_ int) error {
	return errors.New("sendKill only works on Linux")
}
