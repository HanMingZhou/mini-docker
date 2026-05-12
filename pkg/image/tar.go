package image

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// extractTar 解压 .tar 或 .tar.gz 到 dst 目录。
// 只处理常规文件、目录、符号链接、硬链接；特殊设备节点跳过。
// 会拒绝包含 ../ 逃逸的条目。
//
// 注意：这个实现故意不处理 whiteout (.wh.*)——Level 2 的镜像只有单层，
// whiteout 留给 Level 3 的 OCI layer 解包实现。
func extractTar(tarPath, dst string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	var rd io.Reader = f
	// 根据后缀自动解压 gzip
	if strings.HasSuffix(strings.ToLower(tarPath), ".gz") ||
		strings.HasSuffix(strings.ToLower(tarPath), ".tgz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		rd = gz
	}

	tr := tar.NewReader(rd)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		target, err := safeJoin(dst, hdr.Name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
				os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			// symlink 的 Linkname 可以是相对或绝对路径，交给 os.Symlink 原样处理
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			linkSrc, err := safeJoin(dst, hdr.Linkname)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Link(linkSrc, target); err != nil {
				return err
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			// 特殊文件（设备节点、FIFO）在非 root 下没法创建；
			// 容器 rootfs 通常不需要这些（/dev 是 tmpfs），直接跳过。
			continue
		default:
			// 未知类型跳过
			continue
		}
	}
}

// safeJoin 把 name 拼到 base 下，并防止 ../ 逃逸。
// tar 条目名是 POSIX 风格；我们在拼接前先按 "/" 分段检查是否包含 ".."。
func safeJoin(base, name string) (string, error) {
	// tar header 可能以 / 开头（绝对路径），也可能混用反斜杠（异常归档）
	normalized := strings.ReplaceAll(name, "\\", "/")
	segs := strings.Split(strings.Trim(normalized, "/"), "/")
	for _, s := range segs {
		if s == ".." {
			return "", fmt.Errorf("tar entry escapes target: %q", name)
		}
	}
	// 拼路径时使用当前平台分隔符
	final := base
	for _, s := range segs {
		if s == "" || s == "." {
			continue
		}
		final = filepath.Join(final, s)
	}
	return final, nil
}
