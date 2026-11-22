package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/AitorConS/jerboa/internal/agent"
	"github.com/AitorConS/jerboa/internal/config"
	hypervisorpkg "github.com/AitorConS/jerboa/internal/hypervisor"
	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var cfg agent.Config
	cmd := &cobra.Command{
		Use:     "jerboa-agent",
		Short:   "Jerboa Desktop local HTTP/SSE sidecar",
		Version: version,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			token := os.Getenv("JERBOA_AGENT_TOKEN")
			if token == "" {
				return fmt.Errorf("JERBOA_AGENT_TOKEN is required")
			}
			cfg.Token = token
			cfg.Version = version
			endpoint, err := agent.ResolveEndpoint(cfg.Endpoint)
			if err != nil {
				return err
			}
			cfg.Endpoint = endpoint
			if tok := config.ResolveToken(); tok != "" {
				_ = os.Setenv("JERBOA_AUTH_TOKEN", tok)
			}
			if cfg.Hypervisor == "" {
				if c, err := config.Load(config.DefaultPath()); err == nil {
					cfg.Hypervisor = c.Hypervisor
				}
			}
			return run(cmd.Context(), cfg)
		},
	}
	cmd.Flags().StringVar(&cfg.Listen, "listen", "127.0.0.1:0", "HTTP listen address")
	cmd.Flags().StringVar(&cfg.Endpoint, "endpoint", "", "jerboad daemon endpoint (unix:///path or tcp://host:port)")
	cmd.Flags().StringVar(&cfg.Distro, "distro", "", "WSL distro for daemon launch")
	cmd.Flags().StringVar(&cfg.User, "user", "", "WSL user for daemon launch")
	cmd.Flags().BoolVar(&cfg.Sudo, "sudo", false, "run jerboad through sudo inside WSL")
	cmd.Flags().StringVar(&cfg.Hypervisor, "hypervisor", "", "hypervisor to run ("+hypervisorpkg.Values()+")")
	cmd.Flags().StringVar(&cfg.JerboadPath, "jerboad-path", "", "jerboad binary path inside WSL")
	if runtime.GOOS == "windows" && cfg.User == "" {
		cfg.Distro = agent.DefaultWindowsDistro()
		cfg.User = "root"
	}
	return cmd
}

func run(parent context.Context, cfg agent.Config) error {
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		cancel()
	}()

	srv := agent.NewServer(cfg)
	ln, err := srv.Listen()
	if err != nil {
		return err
	}
	ready := struct {
		Addr    string `json:"addr"`
		Version string `json:"version"`
	}{Addr: ln.Addr().String(), Version: cfg.Version}
	line, err := json.Marshal(ready)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("marshal ready line: %w", err)
	}
	fmt.Println(string(line))
	return srv.Serve(ctx, ln)
}
