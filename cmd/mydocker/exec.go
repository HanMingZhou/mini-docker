package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/mini-docker/mini-docker/pkg/nsenter"
	"github.com/mini-docker/mini-docker/pkg/store"
)

// newExecCmd 在一个运行中的容器里执行命令，语义与 `docker exec` 类似。
// 实现细节见 pkg/nsenter。
func newExecCmd() *cobra.Command {
	var tty bool
	cmd := &cobra.Command{
		Use:   "exec [flags] <id|name> -- <cmd> [args...]",
		Short: "Run a command inside a running container",
		Long: `exec enters a running container's namespaces (via setns) and executes
the given command there. The container must be running.`,
		Example: `  mydocker exec -it web -- /bin/sh
  mydocker exec web -- ps aux`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			argv := args[1:]

			st, err := store.New(store.Root())
			if err != nil {
				return err
			}
			c, err := st.Resolve(ref)
			if err != nil {
				return err
			}
			if c.State != store.StateRunning || !pidAlive(c.PID) {
				return fmt.Errorf("container %s is not running", c.ID)
			}

			code, err := nsenter.SpawnViaSelf(
				nsenter.Target{TargetPID: c.PID, Namespaces: nsenter.DefaultNamespaces()},
				argv,
				tty,
			)
			if err != nil {
				return err
			}
			if code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "attach stdio (recommended for interactive shells)")
	// 和 run 一样提供 -i alias 但目前未真正使用
	cmd.Flags().BoolP("interactive", "i", false, "keep STDIN open (currently implied by -t)")
	_ = cmd.Flags().MarkHidden("interactive")
	return cmd
}

// newNsexecCmd 是一个隐藏的内部子命令，由父进程通过 /proc/self/exe nsexec
// 触发，负责进入目标容器的 namespace 并 execve 用户命令。
func newNsexecCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "nsexec",
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return nsenter.EnterAndExec()
		},
	}
}
