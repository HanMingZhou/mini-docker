package image

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// 写一个包含普通文件、目录、符号链接的 tar，然后解压验证。
func TestExtractTarBasic(t *testing.T) {
	tmp := t.TempDir()
	tarPath := filepath.Join(tmp, "x.tar")
	dst := filepath.Join(tmp, "out")

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeHeader(t, tw, &tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0755})
	writeHeader(t, tw, &tar.Header{Name: "dir/hello.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 5})
	if _, err := tw.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		writeHeader(t, tw, &tar.Header{
			Name: "dir/link", Typeflag: tar.TypeSymlink, Linkname: "hello.txt", Mode: 0777,
		})
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tarPath, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	if err := extractTar(tarPath, dst); err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "dir/hello.txt")); err != nil || string(got) != "hello" {
		t.Fatalf("hello.txt = %q, err=%v", got, err)
	}
	if runtime.GOOS != "windows" {
		link, err := os.Readlink(filepath.Join(dst, "dir/link"))
		if err != nil || link != "hello.txt" {
			t.Fatalf("link = %q, err=%v", link, err)
		}
	}
}

// 带 ../ 的路径必须被拒绝。
func TestExtractTarRejectsEscape(t *testing.T) {
	tmp := t.TempDir()
	tarPath := filepath.Join(tmp, "x.tar")
	dst := filepath.Join(tmp, "out")

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeHeader(t, tw, &tar.Header{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0644, Size: 4})
	if _, err := tw.Write([]byte("evil")); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = os.WriteFile(tarPath, buf.Bytes(), 0644)

	err := extractTar(tarPath, dst)
	if err == nil {
		t.Fatal("expected error on escaping path")
	}
}

func writeHeader(t *testing.T, tw *tar.Writer, h *tar.Header) {
	t.Helper()
	if err := tw.WriteHeader(h); err != nil {
		t.Fatalf("WriteHeader %q: %v", h.Name, err)
	}
}

func TestFormatLowerDirs(t *testing.T) {
	got := formatLowerDirs([]string{"/a", "/b", "/c"})
	// 入参顺序 = 下到上；overlay lowerdir = 上到下
	want := "/c:/b:/a"
	if got != want {
		t.Errorf("formatLowerDirs = %q, want %q", got, want)
	}
}
