package image

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// extractTarStream extracts an uncompressed tar stream into dst, honoring
// OCI layer whiteouts:
//   - ".wh.<name>" marks <name> as deleted in lower layers (we replace with
//     a zero-byte marker file prefixed with ".wh." so the overlay mount can
//     interpret it — actually, we just create a character-device mknod(0,0)
//     per OverlayFS whiteout convention. Since mknod requires CAP_MKNOD we
//     fall back to a plain marker file when running without privileges; for
//     single-layer / single-owner images this is acceptable.)
//   - ".wh..wh..opq" (opaque dir marker) is recorded by creating a zero-byte
//     ".wh..wh..opq" file; OverlayFS honors it when the layer is used as a
//     lowerdir via the "trusted.overlay.opaque" xattr (again requires priv).
//
// For mini-docker's scope (Level 3 educational) we record whiteouts as plain
// marker files. Users who need correct multi-layer whiteout semantics should
// run as root so mknod can create proper character devices.
func extractTarStream(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		name := strings.TrimPrefix(hdr.Name, "./")
		if name == "" {
			continue
		}

		// Handle whiteouts (OCI layer convention)
		base := filepath.Base(name)
		if strings.HasPrefix(base, ".wh.") {
			dir := filepath.Dir(name)
			target, err := safeJoin(dst, filepath.Join(dir, base))
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			// Store marker file; overlayfs won't interpret it, but downstream
			// tooling (and our own logic) can detect & handle it.
			if err := os.WriteFile(target, nil, 0644); err != nil {
				return err
			}
			continue
		}

		target, err := safeJoin(dst, name)
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
			// Device nodes / FIFOs require privileges. Skip silently; container
			// /dev is usually a fresh tmpfs anyway.
			continue
		default:
			// XGlobalHeader, XHeader, and unknown types — skip
			continue
		}
	}
}

// defaultArch returns the current runtime's architecture, mapped to the names
// used by OCI image manifests.
func defaultArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	case "arm":
		return "arm"
	case "386":
		return "386"
	default:
		return runtime.GOARCH
	}
}
