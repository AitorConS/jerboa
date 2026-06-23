package api

import "testing"

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		name        string
		endpoint    string
		wantNetwork string
		wantAddress string
		wantErr     bool
	}{
		{"unix scheme", "unix:///var/run/unid.sock", "unix", "/var/run/unid.sock", false},
		{"tcp scheme", "tcp://127.0.0.1:7890", "tcp", "127.0.0.1:7890", false},
		{"bare path is unix", "/tmp/unid.sock", "unix", "/tmp/unid.sock", false},
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
