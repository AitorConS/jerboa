//go:build ignore

// Fetch only the signed guest boot artifacts for explicit x86 compatibility.
// Host mkfs remains the native Mach-O ARM64 binary. Never download Linux tools
// over the native tool directory.
//
// This is for local builds. Release candidates stage their own selected x86
// toolset instead (candidate.py x86 in release.yml). An optional expected
// kernel version refuses any other stable manifest, including an older one a
// machine without release state would otherwise accept.
package main

import (
	"context"
	"fmt"
	"github.com/AitorConS/jerboa/internal/release"
	"github.com/AitorConS/jerboa/internal/tools"
	"os"
	"path/filepath"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) != 2 && len(os.Args) != 3 {
		return fmt.Errorf("usage: go run scripts/fetch-x86-tools.go NATIVE_TOOLS_DIRECTORY [KERNEL_VERSION]")
	}
	root := os.Args[1]
	want := ""
	if len(os.Args) == 3 {
		want = os.Args[2]
	}
	if err := tools.ValidateNativeTools(root); err != nil {
		return err
	}
	cl, err := release.Default()
	if err != nil {
		return err
	}
	ctx := context.Background()
	component, err := tools.KernelComponentFromManifest(ctx, cl, release.ChannelStable)
	if err != nil {
		return err
	}
	if want != "" && component.Version != want {
		return fmt.Errorf("stable kernel is %s, expected %s", component.Version, want)
	}
	stage, err := os.MkdirTemp(root, ".x86-staging-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for _, name := range []string{"boot.img", "kernel.img"} {
		asset, ok := component.Files[name]
		if !ok {
			return fmt.Errorf("signed manifest lacks %s", name)
		}
		if err := cl.DownloadArtifact(ctx, asset, filepath.Join(stage, name)); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(stage, "kernel-version.txt"), []byte(component.Version+"\n"), 0600); err != nil {
		return err
	}
	dest := filepath.Join(root, "x86")
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("%s already exists; select an empty tools directory to replace it", dest)
	}
	if err := os.Rename(stage, dest); err != nil {
		return err
	}
	fmt.Println("Verified x86 guest artifacts installed at", dest)
	return nil
}
