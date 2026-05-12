package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mini-docker/mini-docker/pkg/image"
	"github.com/mini-docker/mini-docker/pkg/store"
)

func newImageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Manage local images",
	}
	cmd.AddCommand(
		newImagePullCmd(),
		newImagePushCmd(),
		newImageImportCmd(),
		newImageListCmd(),
		newImageRmCmd(),
		newImageTagCmd(),
		newImageSaveCmd(),
		newImageLoadCmd(),
	)
	return cmd
}

func newImagePullCmd() *cobra.Command {
	var platform string
	cmd := &cobra.Command{
		Use:   "pull <reference>",
		Short: "Pull an image from a remote registry",
		Long: `Pull an image from a remote registry. The reference follows standard
Docker/OCI conventions (nginx, nginx:alpine, gcr.io/foo/bar:v1, etc.).
Authentication uses ~/.docker/config.json by default.`,
		Example: `  mydocker image pull nginx
  mydocker image pull nginx:alpine
  mydocker image pull --platform linux/arm64 alpine:3.19`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			is, err := image.New(store.Root())
			if err != nil {
				return err
			}
			m, err := is.Pull(args[0], image.PullOptions{
				Platform: platform,
				Progress: func(msg string) {
					fmt.Fprintln(os.Stderr, msg)
				},
			})
			if err != nil {
				return err
			}
			fmt.Println(m.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&platform, "platform", "", "target platform (e.g. linux/amd64, linux/arm64)")
	return cmd
}

func newImageImportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import <name> <tar|tar.gz>",
		Short: "Import a rootfs tarball as a single-layer image",
		Example: `  # Export a rootfs from docker, then import it as 'ubuntu'
  docker export $(docker create ubuntu:22.04) -o ubuntu.tar
  mydocker image import ubuntu ubuntu.tar`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			is, err := image.New(store.Root())
			if err != nil {
				return err
			}
			if err := is.Import(args[0], args[1]); err != nil {
				return err
			}
			fmt.Printf("imported image: %s\n", args[0])
			return nil
		},
	}
}

func newImageListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List imported images",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			is, err := image.New(store.Root())
			if err != nil {
				return err
			}
			list, err := is.List()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "IMAGE\tREFERENCE\tLAYERS\tSIZE\tCREATED")
			for _, m := range list {
				nLayers := len(m.Layers)
				if n := len(m.LayerRefs); n > nLayers {
					nLayers = n
				}
				ref := m.Reference
				if ref == "" {
					ref = "-"
				}
				size := "-"
				if m.Size > 0 {
					size = humanSizeBytes(m.Size)
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", m.Name, ref, nLayers, size, humanAge(m.CreatedAt))
			}
			return w.Flush()
		},
	}
}

func humanSizeBytes(n int64) string {
	const (
		kb = 1 << 10
		mb = 1 << 20
		gb = 1 << 30
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.2fGB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.2fMB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.2fKB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func newImageRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name> [name...]",
		Short: "Remove one or more images",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			is, err := image.New(store.Root())
			if err != nil {
				return err
			}
			for _, n := range args {
				// Resolve to manifest first so short/full/digest all work.
				m, err := is.Resolve(n)
				if err != nil {
					return err
				}
				if m == nil {
					return fmt.Errorf("no such image: %s", n)
				}
				if err := is.Remove(m.Name); err != nil {
					return err
				}
				fmt.Println(n)
			}
			return nil
		},
	}
}
