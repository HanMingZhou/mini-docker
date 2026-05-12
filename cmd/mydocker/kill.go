package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/mini-docker/mini-docker/pkg/cgroup"
	"github.com/mini-docker/mini-docker/pkg/store"
)

func newKillCmd() *cobra.Command {
	var sig string
	cmd := &cobra.Command{
		Use:   "kill [--signal SIG] <id|name>",
		Short: "Send a signal to a running container (default SIGKILL)",
		Long: `Send a signal to the container's init process.
Unlike 'stop' which gracefully terminates (SIGTERM then SIGKILL), kill sends
the signal immediately and does not wait.`,
		Example: `  mydocker kill my-container                  # SIGKILL
  mydocker kill -s HUP nginx                  # SIGHUP for config reload
  mydocker kill -s SIGTERM web`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return killContainer(args[0], sig)
		},
	}
	cmd.Flags().StringVarP(&sig, "signal", "s", "KILL", "signal to send: name (HUP/TERM/KILL/USR1...) or number")
	return cmd
}

func killContainer(ref, sig string) error {
	signum, err := parseSignal(sig)
	if err != nil {
		return err
	}
	st, err := store.New(store.Root())
	if err != nil {
		return err
	}
	c, err := st.Resolve(ref)
	if err != nil {
		return err
	}
	if c.State != store.StateRunning && c.State != store.StatePaused {
		return fmt.Errorf("container %s is not running (state: %s)", c.ID, c.State)
	}
	if !pidAlive(c.PID) {
		return fmt.Errorf("container %s PID %d is not alive", c.ID, c.PID)
	}
	if err := syscall.Kill(c.PID, signum); err != nil {
		return fmt.Errorf("kill pid %d: %w", c.PID, err)
	}
	fmt.Println(c.ID)
	return nil
}

// parseSignal parses "KILL", "SIGKILL", "9", "SIGRTMIN+3" (limited). Returns
// syscall.Signal value.
func parseSignal(s string) (syscall.Signal, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	// Strip optional SIG prefix
	s = strings.TrimPrefix(s, "SIG")
	// Numeric
	if n, err := strconv.Atoi(s); err == nil {
		if n <= 0 || n >= 64 {
			return 0, fmt.Errorf("signal number out of range: %d", n)
		}
		return syscall.Signal(n), nil
	}
	// Named
	if sig, ok := sigNameMap[s]; ok {
		return sig, nil
	}
	return 0, fmt.Errorf("unknown signal: %s", s)
}

var sigNameMap = map[string]syscall.Signal{
	"HUP":    syscall.SIGHUP,
	"INT":    syscall.SIGINT,
	"QUIT":   syscall.SIGQUIT,
	"ILL":    syscall.SIGILL,
	"TRAP":   syscall.SIGTRAP,
	"ABRT":   syscall.SIGABRT,
	"BUS":    syscall.SIGBUS,
	"FPE":    syscall.SIGFPE,
	"KILL":   syscall.SIGKILL,
	"USR1":   syscall.SIGUSR1,
	"SEGV":   syscall.SIGSEGV,
	"USR2":   syscall.SIGUSR2,
	"PIPE":   syscall.SIGPIPE,
	"ALRM":   syscall.SIGALRM,
	"TERM":   syscall.SIGTERM,
	"CHLD":   syscall.SIGCHLD,
	"CONT":   syscall.SIGCONT,
	"STOP":   syscall.SIGSTOP,
	"TSTP":   syscall.SIGTSTP,
	"TTIN":   syscall.SIGTTIN,
	"TTOU":   syscall.SIGTTOU,
	"URG":    syscall.SIGURG,
	"XCPU":   syscall.SIGXCPU,
	"XFSZ":   syscall.SIGXFSZ,
	"VTALRM": syscall.SIGVTALRM,
	"PROF":   syscall.SIGPROF,
	"WINCH":  syscall.SIGWINCH,
	"IO":     syscall.SIGIO,
	"SYS":    syscall.SIGSYS,
}

// --- pause / unpause -------------------------------------------------------

func newPauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause <id|name>",
		Short: "Suspend all processes in a running container",
		Long: `Freezes a container using the cgroup v2 freezer. Processes are
stopped until 'mydocker unpause' is called.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return pauseContainer(args[0], true)
		},
	}
}

func newUnpauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unpause <id|name>",
		Short: "Resume a paused container",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return pauseContainer(args[0], false)
		},
	}
}

func pauseContainer(ref string, freeze bool) error {
	st, err := store.New(store.Root())
	if err != nil {
		return err
	}
	c, err := st.Resolve(ref)
	if err != nil {
		return err
	}
	if freeze {
		if c.State == store.StatePaused {
			fmt.Fprintf(os.Stderr, "container %s is already paused\n", c.ID)
			return nil
		}
		if c.State != store.StateRunning {
			return fmt.Errorf("container %s is not running (state: %s)", c.ID, c.State)
		}
	} else {
		if c.State != store.StatePaused {
			fmt.Fprintf(os.Stderr, "container %s is not paused\n", c.ID)
			return nil
		}
	}
	if c.CgroupPath == "" {
		return fmt.Errorf("container %s has no cgroup recorded", c.ID)
	}

	// Rebuild cgroup manager pointing at the same path. We don't need to
	// know the driver here — both drivers support Freeze by writing
	// cgroup.freeze, and we infer the name/parent from stored fields.
	cgName := c.Name
	if cgName == "" {
		cgName = c.ID
	}
	cg, err := cgroup.NewWithConfig(cgroup.Config{
		Driver: cgroup.Driver(c.CgroupDriver),
		Name:   cgName,
		Parent: c.CgroupParent,
	})
	if err != nil {
		return fmt.Errorf("attach cgroup: %w", err)
	}
	if err := cg.Freeze(freeze); err != nil {
		return fmt.Errorf("freeze=%v: %w", freeze, err)
	}

	// Update state
	if freeze {
		c.State = store.StatePaused
	} else {
		c.State = store.StateRunning
	}
	if err := st.Save(c); err != nil {
		return err
	}
	fmt.Println(c.ID)
	return nil
}
