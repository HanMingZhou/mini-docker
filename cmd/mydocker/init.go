package main

import (
	"github.com/spf13/cobra"

	"github.com/mini-docker/mini-docker/pkg/container"
)

// newInitCmd 返回内部使用的 init 子命令。
// 它由 `run` 触发的 /proc/self/exe init 自启动，不接收参数，
// 所有数据通过 FD 3 的 pipe 从父进程传入。
func newInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "init",
		Short:  "internal: container init entrypoint",
		Hidden: true,
		// DisableFlagParsing 避免 cobra 在 exec 后把残留参数当 flag 解析
		DisableFlagParsing: true,
		Args:               cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return container.Init()
		},
	}
}
