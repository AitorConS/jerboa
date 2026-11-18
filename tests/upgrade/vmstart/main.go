// Command vmstart starts a stopped VM again through the daemon's API, as
// Jerboa Desktop does: the upgrade test uses it to bring back the VMs an
// upgrade stopped, because the CLI has no command for it.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/config"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: vmstart VM")
		os.Exit(2)
	}
	if err := start(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "vmstart:", err)
		os.Exit(1)
	}
}

func start(id string) error {
	// The endpoint and token the installer wrote for the CLI.
	config.SetSessionToken(config.ResolveToken())
	client, err := api.Dial(config.ResolveEndpoint(""))
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	info, err := client.Start(ctx, id)
	if err != nil {
		return fmt.Errorf("start %s: %w", id, err)
	}
	fmt.Println(info.ID, info.State)
	return nil
}
