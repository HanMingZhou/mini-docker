package main

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mini-docker/mini-docker/pkg/image"
	"github.com/mini-docker/mini-docker/pkg/store"
)

// saveFormatVersion is stored in manifest.json inside the tar so load can
// validate compatibility.
const saveFormatVersion = "mydocker-save/v1"

// saveIndex is the top-level manifest written inside the archive.
type saveIndex struct {
	FormatVersion string          `json:"format_version"`
	Image         *image.Manifest `json:"image"`
	Layers        []savedLayer    `json:"layers"`
	CreatedAt     time.Time       `json:"created_at"`
}

type savedLayer struct {
	// Key is the layer directory key under <root>/layers/ (for LayerRefs)
	// or the relative path under images/<name>/ (for Layers).
	Key string `json:"key"`
	// InArchive is the path inside the archive where this layer's tar is stored.
	InArchive string `json:"in_archive"`
}

// --- save ------------------------------------------------------------------

func newImageSaveCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "save [flags] <image>",
		Short: "Save an image to a tar archive",
		Long: `Save produces a tar archive containing the image manifest and all
its layers, suitable for 'mydocker image load' on another host.

The output format is mydocker-specific, not docker-compatible.`,
		Example: `  mydocker image save -o nginx.tar nginx:alpine
  mydocker image save busybox > busybox.tar`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return saveImage(args[0], output)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "write to file instead of stdout")
	return cmd
}

func saveImage(ref, output string) error {
	is, err := image.New(store.Root())
	if err != nil {
		return err
	}
	m, err := is.Resolve(ref)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("no such image: %s", ref)
	}

	var out io.Writer
	if output == "" {
		out = os.Stdout
	} else {
		f, err := os.Create(output)
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	}

	tw := tar.NewWriter(out)
	defer tw.Close()

	idx := saveIndex{
		FormatVersion: saveFormatVersion,
		Image:         m,
		CreatedAt:     time.Now().UTC(),
	}

	// Tar each layer directory into a sub-tar within the archive
	for i, rel := range m.LayerRefs {
		src := filepath.Join(store.Root(), rel)
		key := filepath.Base(rel)
		entry := fmt.Sprintf("layers/%s.tar", key)
		if err := tarDirIntoArchive(tw, entry, src); err != nil {
			return fmt.Errorf("tar layer %d (%s): %w", i, key, err)
		}
		idx.Layers = append(idx.Layers, savedLayer{Key: rel, InArchive: entry})
	}
	for i, rel := range m.Layers {
		src := filepath.Join(is.ImageDir(m.Name), rel)
		// Key for Layers is the path under images/<name>/
		key := strings.ReplaceAll(rel, "/", "_")
		entry := fmt.Sprintf("legacy-layers/%s.tar", key)
		if err := tarDirIntoArchive(tw, entry, src); err != nil {
			return fmt.Errorf("tar layer %d (legacy %s): %w", i, rel, err)
		}
		idx.Layers = append(idx.Layers, savedLayer{Key: rel, InArchive: entry})
	}

	// Write the manifest last (so it can reference exact in-archive paths)
	manifestData, _ := json.MarshalIndent(idx, "", "  ")
	if err := tw.WriteHeader(&tar.Header{
		Name:    "manifest.json",
		Mode:    0644,
		Size:    int64(len(manifestData)),
		ModTime: time.Now(),
	}); err != nil {
		return err
	}
	if _, err := tw.Write(manifestData); err != nil {
		return err
	}

	if output != "" {
		fmt.Fprintf(os.Stderr, "saved %s → %s\n", ref, output)
	}
	return nil
}

// tarDirIntoArchive walks src and writes its tree as a nested tar inside tw.
// The nested tar is stored as a single archive entry named by entryName.
//
// Detects hardlinks via inode and emits them as tar.TypeLink (pointing to the
// first occurrence) so that multi-link file systems like busybox (where every
// utility is a hardlink to /bin/busybox) don't blow up the archive.
func tarDirIntoArchive(outer *tar.Writer, entryName, src string) error {
	var buf strBuf
	innerTw := tar.NewWriter(&buf)

	seen := map[uint64]string{} // inode → first relative path

	err := filepath.Walk(src, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		rel, _ := filepath.Rel(src, path)
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel

		// Symlinks
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			hdr.Linkname = link
			if err := innerTw.WriteHeader(hdr); err != nil {
				return err
			}
			return nil
		}

		// Hardlink detection: if we've already tarred this inode, emit as link.
		if info.Mode().IsRegular() {
			if stat, ok := info.Sys().(*syscallStatT); ok && stat != nil && stat.Nlink > 1 {
				ino := stat.Ino
				if first, seenBefore := seen[ino]; seenBefore {
					hdr.Typeflag = tar.TypeLink
					hdr.Linkname = first
					hdr.Size = 0
					return innerTw.WriteHeader(hdr)
				}
				seen[ino] = rel
			}
		}

		if err := innerTw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(innerTw, f)
			_ = f.Close()
			if copyErr != nil {
				return copyErr
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := innerTw.Close(); err != nil {
		return err
	}

	// Now write the inner tar as a single regular file in the outer tar.
	data := buf.Bytes()
	if err := outer.WriteHeader(&tar.Header{
		Name:    entryName,
		Mode:    0644,
		Size:    int64(len(data)),
		ModTime: time.Now(),
	}); err != nil {
		return err
	}
	_, err = outer.Write(data)
	return err
}

// strBuf is a growable byte buffer (avoids pulling bytes package name clashes).
type strBuf struct{ b []byte }

func (s *strBuf) Write(p []byte) (int, error) { s.b = append(s.b, p...); return len(p), nil }
func (s *strBuf) Bytes() []byte               { return s.b }

// --- load ------------------------------------------------------------------

func newImageLoadCmd() *cobra.Command {
	var input string
	cmd := &cobra.Command{
		Use:   "load",
		Short: "Load an image from a tar archive (created by 'save')",
		Example: `  mydocker image load -i nginx.tar
  mydocker image load < busybox.tar`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return loadImage(input)
		},
	}
	cmd.Flags().StringVarP(&input, "input", "i", "", "read from file instead of stdin")
	return cmd
}

func loadImage(input string) error {
	var in io.Reader
	if input == "" {
		in = os.Stdin
	} else {
		f, err := os.Open(input)
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}

	// First pass: collect all entries into a map so we can process manifest
	// in any order.
	tr := tar.NewReader(in)
	entries := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		data := make([]byte, hdr.Size)
		if _, err := io.ReadFull(tr, data); err != nil {
			return fmt.Errorf("read entry %s: %w", hdr.Name, err)
		}
		entries[hdr.Name] = data
	}

	raw, ok := entries["manifest.json"]
	if !ok {
		return fmt.Errorf("archive missing manifest.json (not a mydocker save?)")
	}
	var idx saveIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return fmt.Errorf("decode manifest.json: %w", err)
	}
	if idx.FormatVersion != saveFormatVersion {
		return fmt.Errorf("unsupported format: %q (want %q)", idx.FormatVersion, saveFormatVersion)
	}
	if idx.Image == nil {
		return fmt.Errorf("manifest has no image data")
	}

	is, err := image.New(store.Root())
	if err != nil {
		return err
	}
	if is.Exists(idx.Image.Name) {
		return fmt.Errorf("image %q already exists; remove it first", idx.Image.Name)
	}

	// Restore layers. For LayerRefs, put under <root>/layers/<key>; for
	// Layers, put under the image's own dir.
	for _, sl := range idx.Layers {
		data, ok := entries[sl.InArchive]
		if !ok {
			return fmt.Errorf("missing layer entry: %s", sl.InArchive)
		}
		var dstDir string
		if strings.HasPrefix(sl.Key, "layers/") {
			dstDir = filepath.Join(store.Root(), sl.Key)
		} else {
			dstDir = filepath.Join(is.ImageDir(idx.Image.Name), sl.Key)
		}
		// Skip if layer already exists (shared with another image)
		if _, err := os.Stat(dstDir); err == nil {
			continue
		}
		if err := os.MkdirAll(dstDir, 0755); err != nil {
			return err
		}
		if err := extractTarBytesToDir(data, dstDir); err != nil {
			_ = os.RemoveAll(dstDir)
			return fmt.Errorf("extract layer %s: %w", sl.Key, err)
		}
	}

	// Finally write the manifest
	if err := is.SaveManifest(idx.Image); err != nil {
		return err
	}
	fmt.Printf("loaded %s\n", idx.Image.Name)
	return nil
}

// extractTarBytesToDir extracts a tar byte-slice into dst.
func extractTarBytesToDir(data []byte, dst string) error {
	tr := tar.NewReader(&readerFromBytes{b: data})
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dst, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			// Hardlink to an earlier entry. Linkname is the relative path
			// inside the archive; resolve it against dst.
			linkSrc := filepath.Join(dst, hdr.Linkname)
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Link(linkSrc, target); err != nil {
				return fmt.Errorf("hardlink %s -> %s: %w", target, linkSrc, err)
			}
		}
	}
}

// readerFromBytes is a minimal bytes reader (to avoid bytes.Buffer name clash).
type readerFromBytes struct {
	b []byte
	i int
}

func (r *readerFromBytes) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}
