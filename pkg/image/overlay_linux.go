//go:build linux

package image

import (
	"fmt"
	"os"
	"syscall"
)

func prepareRootfs(s *Store, imageName, containerDir string) (string, error) {
	if !s.Exists(imageName) {
		return "", fmt.Errorf("no such image: %s", imageName)
	}
	layers, err := s.LayerPaths(imageName)
	if err != nil {
		return "", err
	}

	paths := ContainerRootfsPaths(containerDir)
	for _, d := range []string{paths.Upper, paths.Work, paths.Merged} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return "", fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// overlay mount：数据通过 data 字符串传入
	data := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		formatLowerDirs(layers), paths.Upper, paths.Work)

	if err := syscall.Mount("overlay", paths.Merged, "overlay", 0, data); err != nil {
		// 清理失败时创建的目录，保持幂等
		_ = cleanupRootfs(containerDir)
		return "", fmt.Errorf("mount overlay on %s: %w (data=%q)", paths.Merged, err, data)
	}
	return paths.Merged, nil
}

func cleanupRootfs(containerDir string) error {
	paths := ContainerRootfsPaths(containerDir)

	// umount merged（如果没挂也不是致命错误）
	if err := syscall.Unmount(paths.Merged, syscall.MNT_DETACH); err != nil {
		if !isNotMounted(err) {
			fmt.Fprintf(os.Stderr, "warn: umount %s: %v\n", paths.Merged, err)
		}
	}

	for _, d := range []string{paths.Merged, paths.Work, paths.Upper} {
		if err := os.RemoveAll(d); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warn: remove %s: %v\n", d, err)
		}
	}
	return nil
}

// isNotMounted 判断 umount 的错误是否就是"没挂载"。
func isNotMounted(err error) bool {
	// syscall.EINVAL 通常表示不是挂载点
	if errno, ok := err.(syscall.Errno); ok {
		return errno == syscall.EINVAL || errno == syscall.ENOENT
	}
	return false
}
