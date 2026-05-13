//go:build !linux

package rootfs

import "errors"

func setup(_ string, _ []Mount) error {
	return errors.New("rootfs.Setup only works on Linux")
}

// ApplyBindMounts 在非 Linux 平台是 no-op。
func ApplyBindMounts(_ string, _ []Mount) error {
	return errors.New("rootfs.ApplyBindMounts only works on Linux")
}
