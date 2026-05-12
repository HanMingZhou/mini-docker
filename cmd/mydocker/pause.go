package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// newSandboxPauseCmd 返回一个隐藏的 `sandbox-pause` 子命令。
// 它由 sandbox.Manager 通过 `/proc/self/exe sandbox-pause` 启动，
// 担任 Pod 沙箱里持有 network/UTS/IPC namespace 的"载体进程"。
//
// 语义和官方 pause 镜像一致：不做任何事，就等 SIGTERM/SIGINT 退出。
// 见 https://github.com/kubernetes/kubernetes/tree/master/build/pause
//
// NOTE: 命名上区分于用户可见的 `mydocker pause <container>`。
func newSandboxPauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "sandbox-pause",
		Short:              "internal: Pod sandbox holder process",
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ch := make(chan os.Signal, 1)
			signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
			<-ch
			return nil
		},
	}
}
