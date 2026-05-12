// mydocker-cri 是 mini-docker 的 CRI gRPC 守护进程（Level 3-4）。
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/mini-docker/mini-docker/pkg/cgroup"
	"github.com/mini-docker/mini-docker/pkg/cri"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var (
		socket           string
		streamingAddr    string
		streamingBaseURL string
		cniConfDir       string
		cniBinDirs       []string
		cgroupDriver     string
	)
	root := &cobra.Command{
		Use:   "mydocker-cri",
		Short: "mydocker CRI gRPC server",
		Long: `mydocker-cri implements the Kubernetes Container Runtime Interface
(CRI). It listens on a Unix socket and exposes RuntimeService and ImageService.`,
		SilenceUsage: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&socket, "socket", cri.DefaultSocketPath,
		"unix socket path to listen on")
	pf.StringVar(&streamingAddr, "streaming-addr", "",
		"address for HTTP streaming server (e.g. 127.0.0.1:10350); empty disables Exec/Attach")
	pf.StringVar(&streamingBaseURL, "streaming-base-url", "",
		"optional base URL advertised in streaming URLs (e.g. http://node-ip:10350)")
	pf.StringVar(&cniConfDir, "cni-conf-dir", "/etc/cni/net.d",
		"directory of CNI network configuration files")
	pf.StringSliceVar(&cniBinDirs, "cni-bin-dir", []string{"/opt/cni/bin"},
		"directories to search for CNI plugin binaries (comma-separated or repeated)")
	pf.StringVar(&cgroupDriver, "cgroup-driver", "systemd",
		"cgroup driver: systemd or cgroupfs; must match Kubelet's --cgroup-driver")

	root.AddCommand(newServeCmd(&socket, &streamingAddr, &streamingBaseURL, &cniConfDir, &cniBinDirs, &cgroupDriver))
	root.CompletionOptions.HiddenDefaultCmd = true
	return root
}

func newServeCmd(socket, streamingAddr, streamingBaseURL, cniConfDir *string, cniBinDirs *[]string, cgroupDriver *string) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the CRI gRPC server in the foreground",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			srv, err := cri.New(cri.Config{
				Socket:           *socket,
				StreamingAddr:    *streamingAddr,
				StreamingBaseURL: *streamingBaseURL,
				CNIConfDir:       *cniConfDir,
				CNIBinDirs:       *cniBinDirs,
				CgroupDriver:     cgroup.Driver(*cgroupDriver),
			})
			if err != nil {
				return err
			}

			stop := make(chan os.Signal, 1)
			signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				<-stop
				fmt.Fprintln(os.Stderr, "shutting down...")
				srv.Stop()
			}()

			fmt.Fprintf(os.Stderr, "mydocker-cri listening on %s\n", *socket)
			if *streamingAddr != "" {
				fmt.Fprintf(os.Stderr, "streaming server on %s\n", *streamingAddr)
			}
			fmt.Fprintf(os.Stderr, "cgroup driver: %s\n", *cgroupDriver)
			return srv.Start()
		},
	}
}
