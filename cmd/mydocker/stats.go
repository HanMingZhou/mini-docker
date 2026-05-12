package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mini-docker/mini-docker/pkg/store"
)

func newStatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats [id|name...]",
		Short: "Display resource usage statistics (one-shot)",
		Long: `Show CPU and memory usage for running containers.
Reads cgroup v2 metrics from /sys/fs/cgroup.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return showStats(args)
		},
	}
}

func showStats(refs []string) error {
	st, err := store.New(store.Root())
	if err != nil {
		return err
	}

	var containers []*store.Container
	if len(refs) == 0 {
		all, err := st.List()
		if err != nil {
			return err
		}
		for _, c := range all {
			if c.State == store.StateRunning && pidAlive(c.PID) {
				containers = append(containers, c)
			}
		}
	} else {
		for _, ref := range refs {
			c, err := st.Resolve(ref)
			if err != nil {
				return err
			}
			containers = append(containers, c)
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CONTAINER\tNAME\tCPU (us)\tMEM USAGE\tMEM LIMIT")
	for _, c := range containers {
		cpu := readCgroupStat(c.CgroupPath, "cpu.stat", "usage_usec")
		memCur := readCgroupFile(c.CgroupPath, "memory.current")
		memMax := readCgroupFile(c.CgroupPath, "memory.max")
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			c.ID[:12], c.Name, cpu, humanBytes(memCur), humanBytesOrMax(memMax))
	}
	return w.Flush()
}

func readCgroupFile(cgPath, filename string) string {
	if cgPath == "" {
		return "-"
	}
	data, err := os.ReadFile(filepath.Join(cgPath, filename))
	if err != nil {
		return "-"
	}
	return strings.TrimSpace(string(data))
}

func readCgroupStat(cgPath, filename, key string) string {
	if cgPath == "" {
		return "-"
	}
	data, err := os.ReadFile(filepath.Join(cgPath, filename))
	if err != nil {
		return "-"
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[0] == key {
			return parts[1]
		}
	}
	return "-"
}

func humanBytes(s string) string {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return s
	}
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

func humanBytesOrMax(s string) string {
	if s == "max" || s == "-" {
		return "unlimited"
	}
	return humanBytes(s)
}
