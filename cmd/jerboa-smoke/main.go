//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/apiserver"
	"github.com/AitorConS/jerboa/internal/image"
	"github.com/AitorConS/jerboa/internal/network"
	"github.com/AitorConS/jerboa/internal/service"
	"github.com/AitorConS/jerboa/internal/vm"
)

type result struct {
	name   string
	status string
	detail string
}

func main() {
	os.Exit(run())
}

func run() int {
	var jerboaBin string
	flag.StringVar(&jerboaBin, "jerboa", "", "path to jerboa binary")
	flag.Parse()

	if jerboaBin == "" {
		jerboaBin = filepath.Join(".", "jerboa")
	}

	absBin, err := filepath.Abs(jerboaBin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "smoke setup failed (resolve jerboa path): %v\n", err)
		return 2
	}
	if _, err := os.Stat(absBin); err != nil {
		fmt.Fprintf(os.Stderr, "smoke setup failed (jerboa binary not found): %s: %v\n", absBin, err)
		return 2
	}

	baseDir, err := os.MkdirTemp("", "jerboa-smoke-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "smoke setup failed (create temp dir): %v\n", err)
		return 2
	}
	defer os.RemoveAll(baseDir)

	socketPath := filepath.Join(baseDir, "jerboad.sock")
	storePath := filepath.Join(baseDir, "images")
	homePath := filepath.Join(baseDir, "home")
	if err := os.MkdirAll(homePath, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "smoke setup failed (create temp home): %v\n", err)
		return 2
	}

	if err := seedImageStore(storePath); err != nil {
		fmt.Fprintf(os.Stderr, "smoke setup failed (seed image store): %v\n", err)
		return 2
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	netStore, err := network.NewStore(filepath.Join(homePath, ".jerboa", "networks"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "smoke setup failed (create network store): %v\n", err)
		return 2
	}
	svcStore, err := service.NewFileStore(filepath.Join(homePath, ".jerboa", "services"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "smoke setup failed (create service store): %v\n", err)
		return 2
	}

	mgr := vm.NewQEMUManager("fake-qemu", vm.WithCommandFunc(fakeQEMUCmd()))
	svcMgr := service.NewManager(mgr, svcStore)
	clusterLister := staticClusterLister{}
	srv, err := apiserver.NewServer(mgr, netStore, svcMgr, socketPath, nil, "smoke", clusterLister)
	if err != nil {
		fmt.Fprintf(os.Stderr, "smoke setup failed (start api server): %v\n", err)
		return 2
	}
	go func() {
		_ = srv.Serve(ctx)
	}()
	if err := waitForSocket(socketPath, 5*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "smoke setup failed (wait daemon): %v\n", err)
		return 2
	}

	runUni := func(args ...string) (string, error) {
		base := []string{"--socket", socketPath, "--store", storePath}
		base = append(base, args...)
		cmd := exec.Command(absBin, base...)
		cmd.Env = append(os.Environ(), "HOME="+homePath, "USERPROFILE="+homePath)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		return out.String(), err
	}

	results := make([]result, 0, 40)
	add := func(name string, err error, out string) {
		if err != nil {
			results = append(results, result{name: name, status: "FAIL", detail: trim(out, err.Error())})
			return
		}
		results = append(results, result{name: name, status: "PASS", detail: trim(out)})
	}
	skip := func(name, why string) {
		results = append(results, result{name: name, status: "SKIP", detail: why})
	}

	var vmID string

	out, err := runUni("images")
	add("images", err, out)

	out, err = runUni("run", "hello:latest", "--name", "smoke-vm", "-d")
	if err == nil {
		vmID = strings.TrimSpace(out)
	}
	add("run", err, out)

	addCmd(runUni, add, "ps", "ps")
	addCmd(runUni, add, "status", "status")

	if vmID != "" {
		addCmd(runUni, add, "logs", "logs", vmID)
		addCmd(runUni, add, "inspect", "inspect", vmID)
		addCmd(runUni, add, "stats", "stats", vmID)
		addCmd(runUni, add, "exec", "exec", "--signal", "SIGTERM", vmID)
		addCmd(runUni, add, "stop", "stop", vmID)
		addCmd(runUni, add, "rm", "rm", vmID)
	} else {
		skip("logs/inspect/stats/exec/stop/rm", "run did not produce vm id")
	}

	addCmd(runUni, add, "network create", "network", "create", "smoke-net")
	addCmd(runUni, add, "network ls", "network", "ls")
	addCmd(runUni, add, "network inspect", "network", "inspect", "smoke-net")
	addCmd(runUni, add, "network rm", "network", "rm", "smoke-net")

	addCmd(runUni, add, "volume create", "volume", "create", "smoke-vol", "--size", "32M")
	addCmd(runUni, add, "volume ls", "volume", "ls")
	addCmd(runUni, add, "volume inspect", "volume", "inspect", "smoke-vol")
	addCmd(runUni, add, "volume rm", "volume", "rm", "smoke-vol")

	addCmd(runUni, add, "network create (svc)", "network", "create", "svc-net")
	addCmd(runUni, add, "service run", "service", "run", "web", "hello:latest", "--replicas", "2", "--network", "svc-net")
	addCmd(runUni, add, "service ls", "service", "ls")
	addCmd(runUni, add, "service inspect", "service", "inspect", "web")
	addCmd(runUni, add, "service scale", "service", "scale", "web", "1")
	addCmd(runUni, add, "service update", "service", "update", "web", "hello:latest")
	skip("dns resolve-all", "requires DNS records with assigned VM IPs")
	skip("dns resolve", "requires DNS records with assigned VM IPs")
	addCmd(runUni, add, "service rm", "service", "rm", "web")
	addCmd(runUni, add, "network rm (svc)", "network", "rm", "svc-net")

	addCmd(runUni, add, "dns list", "dns", "list")
	addCmd(runUni, add, "node ls", "node", "ls")

	composeFile := filepath.Join(baseDir, "compose.yml")
	composeYAML := "version: \"1\"\nservices:\n  app:\n    image: hello:latest\n"
	if err := os.WriteFile(composeFile, []byte(composeYAML), 0o600); err != nil {
		skip("compose up/ps/logs/down", "failed to write compose fixture")
	} else {
		out, err := runUni("compose", "up", composeFile)
		add("compose up", err, out)
		if err == nil {
			addCmd(runUni, add, "compose ps", "compose", "ps", composeFile)
			addCmd(runUni, add, "compose logs", "compose", "logs", composeFile, "app")
			addCmd(runUni, add, "compose down", "compose", "down", composeFile)
		} else {
			skip("compose ps/logs/down", "compose up failed")
		}
	}

	addCmd(runUni, add, "rmi", "rmi", "hello:latest")

	skip("build", "requires mkfs/kernel artifacts in host environment")
	addCmd(runUni, add, "pkg list", "pkg", "list")
	skip("pkg search/get/push", "package index/network dependent")
	skip("kernel", "remote kernel release interaction")

	printResults(results)

	if hasFail(results) {
		return 1
	}
	return 0
}

func addCmd(run func(...string) (string, error), add func(string, error, string), name string, args ...string) {
	out, err := run(args...)
	add(name, err, out)
}

func seedImageStore(storePath string) error {
	if err := os.MkdirAll(storePath, 0o755); err != nil {
		return fmt.Errorf("create image store dir: %w", err)
	}
	store, err := image.NewStore(storePath)
	if err != nil {
		return fmt.Errorf("open image store: %w", err)
	}
	disk := filepath.Join(storePath, "hello-disk.img")
	if err := os.WriteFile(disk, []byte("fake-disk"), 0o600); err != nil {
		return fmt.Errorf("write disk image: %w", err)
	}
	m := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          "hello",
		Tag:           "latest",
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    "sha256:abc123",
		DiskSize:      int64(len("fake-disk")),
		Created:       time.Now(),
	}
	return store.Put("hello", "latest", m, disk)
}

func fakeQEMUCmd() vm.CommandFunc {
	return func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.Command("sleep", "3600")
	}
}

func waitForSocket(socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		client, err := api.Dial(socketPath)
		if err == nil {
			_ = client.Close()
			return nil
		}
		time.Sleep(30 * time.Millisecond)
	}
	return errors.New("daemon socket did not become ready")
}

func printResults(results []result) {
	pass := 0
	failCount := 0
	skip := 0
	for _, r := range results {
		switch r.status {
		case "PASS":
			pass++
		case "FAIL":
			failCount++
		case "SKIP":
			skip++
		}
		fmt.Printf("[%s] %s\n", r.status, r.name)
		if r.detail != "" {
			fmt.Printf("  %s\n", r.detail)
		}
	}
	fmt.Printf("\nSummary: PASS=%d FAIL=%d SKIP=%d\n", pass, failCount, skip)
}

func hasFail(results []result) bool {
	for _, r := range results {
		if r.status == "FAIL" {
			return true
		}
	}
	return false
}

func trim(parts ...string) string {
	s := strings.TrimSpace(strings.Join(parts, " "))
	if len(s) > 220 {
		return s[:220] + "..."
	}
	return s
}

type staticClusterLister struct{}

func (staticClusterLister) Members() []apiserver.ClusterMember {
	return []apiserver.ClusterMember{{
		ID:       "smoke-node-1",
		Addr:     "127.0.0.1:7946",
		Status:   "alive",
		VMCount:  0,
		CPUCap:   0,
		MemCap:   0,
		LastSeen: time.Now(),
	}}
}
