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
	/*
		source	"overlay"	对 overlay 来说这个字段没意义（不像普通 bind mount 指向设备或路径），写什么都行，约定写文件系统名
		target	paths.Merged	挂载点——合并视图挂在哪里，容器的 / 就指向这里
		filesystemtype	"overlay"	告诉内核挂的是 overlayfs，不是 ext4/tmpfs 之类
		mountflags	0	不加特殊标志（比如 MS_RDONLY / MS_NOSUID）
		data	下面的大字符串	overlayfs 特有参数，所有重要信息都在这里

		data 字符串的结构
			formatLowerDirs(layers) 把若干镜像层用冒号拼起来，上层在前、下层在后（这是 overlayfs 的约定）：
			lowerdir=/var/lib/mydocker/images/layer4/content:/var/lib/mydocker/images/layer3/content:/var/lib/mydocker/images/layer2/content:/var/lib/mydocker/images/layer1/content
			upperdir=/var/lib/mydocker/containers/abc/upper
			workdir=/var/lib/mydocker/containers/abc/work

		拆开看：
			lowerdir=层4:层3:层2:层1    ← 查找顺序从左到右，越靠前优先级越高
			upperdir=upper              ← 容器独占可写层
			workdir=work                ← 内核工作目录
		执行完 mount 后：
			cat /proc/mounts | grep merged
			# overlay /var/lib/mydocker/containers/abc/merged overlay rw,relatime,lowerdir=...,upperdir=...,workdir=... 0 0
			这时 ls paths.Merged 能看到全部镜像层合并起来的完整文件系统。容器的 init 进程接下来用 pivot_root(merged, ...) 把这里切成新的 /。
	*/
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
