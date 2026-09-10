package api

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		name        string
		endpoint    string
		wantNetwork string
		wantAddress string
		wantErr     bool
	}{
		{"unix scheme", "unix:///var/run/jerboad.sock", "unix", "/var/run/jerboad.sock", false},
		{"tcp scheme", "tcp://127.0.0.1:7890", "tcp", "127.0.0.1:7890", false},
		{"bare path is unix", "/tmp/jerboad.sock", "unix", "/tmp/jerboad.sock", false},
		{"empty", "", "", "", true},
		{"empty unix path", "unix://", "", "", true},
		{"empty tcp addr", "tcp://", "", "", true},
		{"unsupported scheme", "http://localhost:80", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			network, address, err := parseEndpoint(tt.endpoint)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseEndpoint(%q) = nil error, want error", tt.endpoint)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseEndpoint(%q) unexpected error: %v", tt.endpoint, err)
			}
			if network != tt.wantNetwork || address != tt.wantAddress {
				t.Fatalf("parseEndpoint(%q) = (%q, %q), want (%q, %q)",
					tt.endpoint, network, address, tt.wantNetwork, tt.wantAddress)
			}
		})
	}
}

func TestListenUnixSocketOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets are not supported on Windows")
	}
	dir := t.TempDir()
	if runtime.GOOS == "darwin" {
		short, err := os.MkdirTemp("/tmp", "jerboa-socket-test-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(short) })
		dir = short
	}
	socketPath := filepath.Join(dir, "jerboad.sock")
	l, err := Listen("unix://" + socketPath)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer l.Close()

	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket mode = %o, want 600", got)
	}

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial unix socket: %v", err)
	}
	_ = conn.Close()
}
