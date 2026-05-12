package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/mini-docker/mini-docker/pkg/store"
)

func newInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <id|name>",
		Short: "Display detailed information on a container",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := store.New(store.Root())
			if err != nil {
				return err
			}
			c, err := st.Resolve(args[0])
			if err != nil {
				return err
			}
			data, err := json.MarshalIndent(c, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, string(data))
			return nil
		},
	}
}
