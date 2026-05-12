// mydocker 是 mini-docker 项目的 CLI 容器引擎。
//
// 命令行基于 cobra 实现。顶层子命令：
//
//	run / ps / logs / stop / rm / image / init
//
// 其中 init 是内部使用的入口（通过 /proc/self/exe init 自启动），
// 默认从 --help 中隐藏。
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// cobra 自己会把错误打印出来，这里只负责非 0 退出
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "mydocker",
		Short: "mini-docker: a from-scratch container engine",
		Long: `mydocker is the CLI for the mini-docker project.

It creates and manages containers using Linux namespaces, cgroups and overlayfs.`,
		// 不让 cobra 在错误后又打印 usage（noisy）
		SilenceUsage: true,
	}

	// 让 cobra 的全局 Help/Completion 显示与否统一
	root.CompletionOptions.HiddenDefaultCmd = true

	root.AddCommand(
		newRunCmd(),
		newExecCmd(),
		newPsCmd(),
		newLogsCmd(),
		newStopCmd(),
		newKillCmd(),
		newPauseCmd(),
		newUnpauseCmd(),
		newRmCmd(),
		newCpCmd(),
		newCommitCmd(),
		newBuildCmd(),
		newInspectCmd(),
		newStatsCmd(),
		newImageCmd(),
		newInitCmd(),         // Hidden: run 内部使用
		newNsexecCmd(),       // Hidden: exec 内部使用
		newMonitorCmd(),      // Hidden: restart 内部使用
		newSandboxPauseCmd(), // Hidden: sandbox 内部使用（ns 载体进程）
	)
	return root
}
