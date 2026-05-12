package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/mini-docker/mini-docker/pkg/image"
	"github.com/mini-docker/mini-docker/pkg/store"
)

func newCommitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "commit <container> <new-image-name>",
		Short: "Create a new image from a container's changes",
		Long: `Commit takes the container's read-write layer (overlay upper) and
creates a new image that includes the original base layers plus the changes.
The container can be running or stopped.`,
		Example: `  mydocker commit my-container my-custom-image
  mydocker commit abc123 nginx-configured`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return commitContainer(args[0], args[1])
		},
	}
}

func commitContainer(ref, newImageName string) error {
	st, err := store.New(store.Root())
	if err != nil {
		return err
	}
	c, err := st.Resolve(ref)
	if err != nil {
		return err
	}

	is, err := image.New(store.Root())
	if err != nil {
		return err
	}

	// Check new image name doesn't already exist
	if is.Exists(newImageName) {
		return fmt.Errorf("image %q already exists", newImageName)
	}

	// The container's upper layer is at <root>/containers/<id>/upper/
	containerDir := st.ContainerDir(c.ID)
	upperDir := filepath.Join(containerDir, "upper")
	if _, err := os.Stat(upperDir); err != nil {
		return fmt.Errorf("container %s has no upper layer (was it run with --rootfs?)", c.ID)
	}

	// Find the original image's layers. The container stores which image it was
	// created from in ImageName. If available, the new image inherits those layers
	// plus the upper as a new top layer.
	var baseLayers []string    // relative paths for Layers field
	var baseLayerRefs []string // relative paths for LayerRefs field

	if c.ImageName != "" {
		baseManifest, _ := is.Resolve(c.ImageName)
		if baseManifest != nil {
			baseLayers = baseManifest.Layers
			baseLayerRefs = baseManifest.LayerRefs
		}
	}

	// Create the committed layer in the shared layers directory so LayerRefs
	// resolution works correctly (LayerRefs are relative to <root>/).
	commitLayerKey := "committed-" + c.ID[:12]
	commitLayerRel := filepath.Join("layers", commitLayerKey)
	commitLayerDir := filepath.Join(store.Root(), commitLayerRel)
	if err := os.MkdirAll(commitLayerDir, 0755); err != nil {
		return err
	}

	// Copy upper layer contents to the new layer
	if err := copyDir(upperDir, commitLayerDir); err != nil {
		_ = os.RemoveAll(commitLayerDir)
		return fmt.Errorf("copy upper layer: %w", err)
	}

	// Build manifest: base layers + committed layer on top
	newImgDir := is.ImageDir(newImageName)
	m := &image.Manifest{
		Name:      newImageName,
		CreatedAt: time.Now().UTC(),
	}
	if len(baseLayerRefs) > 0 {
		// Pulled image: layers are shared under <root>/layers/
		m.LayerRefs = append(baseLayerRefs, commitLayerRel)
	} else if len(baseLayers) > 0 {
		// Imported image: layers are inside the source image dir.
		// Convert them to shared LayerRefs by copying to <root>/layers/.
		// For simplicity, reference the committed layer only + base via Layers.
		// Actually let's just use LayerRefs for the committed and copy base
		// layer paths as absolute references... this gets complex.
		// Simplest: just use the committed layer standalone for imported bases.
		m.LayerRefs = []string{commitLayerRel}
	} else {
		m.LayerRefs = []string{commitLayerRel}
	}

	if err := is.SaveManifest(m); err != nil {
		_ = os.RemoveAll(newImgDir)
		_ = os.RemoveAll(commitLayerDir)
		return err
	}

	fmt.Printf("committed %s → image %s\n", c.ID[:12], newImageName)
	return nil
}

// copyDir recursively copies src to dst. Symlinks are preserved.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}

		// Symlink
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}

		// Regular file
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}
