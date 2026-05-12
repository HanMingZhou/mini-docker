package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mini-docker/mini-docker/pkg/image"
	"github.com/mini-docker/mini-docker/pkg/store"
)

func newImageTagCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tag <source> <target>",
		Short: "Create a tag (alias) for an image",
		Long: `Tag creates a new image name that points to the same layers as the
source image. Both names will appear in 'mydocker image ls'.`,
		Example: `  mydocker image tag busybox:latest my-busybox
  mydocker image tag nginx:alpine web-server`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return tagImage(args[0], args[1])
		},
	}
}

func tagImage(src, dst string) error {
	is, err := image.New(store.Root())
	if err != nil {
		return err
	}
	srcManifest, err := is.Resolve(src)
	if err != nil {
		return err
	}
	if srcManifest == nil {
		return fmt.Errorf("no such image: %s", src)
	}
	if is.Exists(dst) {
		return fmt.Errorf("image %q already exists", dst)
	}

	// Create a new manifest that shares the same layers
	newM := &image.Manifest{
		Name:      dst,
		Reference: srcManifest.Reference,
		Digest:    srcManifest.Digest,
		Layers:    srcManifest.Layers,
		LayerRefs: srcManifest.LayerRefs,
		Size:      srcManifest.Size,
		CreatedAt: srcManifest.CreatedAt,
	}
	if err := is.SaveManifest(newM); err != nil {
		return err
	}
	fmt.Printf("tagged %s → %s\n", src, dst)
	return nil
}
