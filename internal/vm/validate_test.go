//go:build linux

package vm

import (
	"strings"
	"testing"
)

func TestValidateVMConfig(t *testing.T) {
	base := Config{ImagePath: "img.raw", Memory: "256M"}

	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr bool
	}{
		{"valid minimal", func(*Config) {}, false},
		{"valid full", func(c *Config) {
			c.NetworkName = "jerboa-tap0"
			c.BridgeName = "jerboa-br0"
			c.IPAddress = "10.0.0.2"
			c.GatewayIP = "10.0.0.1"
			c.SubnetMask = "24"
			c.PortMaps = []PortMap{{HostPort: 8080, GuestPort: 80, Protocol: ProtocolTCP}}
		}, false},
		{"missing image", func(c *Config) { c.ImagePath = "" }, true},
		{"missing memory", func(c *Config) { c.Memory = "" }, true},
		{"bad memory", func(c *Config) { c.Memory = "lots" }, true},
		{"memory plain int", func(c *Config) { c.Memory = "512" }, false},
		{"negative cpus", func(c *Config) { c.CPUs = -1 }, true},
		{"bad network name chars", func(c *Config) { c.NetworkName = "tap 0!" }, true},
		{"network name too long", func(c *Config) { c.NetworkName = "a" + strings.Repeat("b", 128) }, true},
		{"bad protocol", func(c *Config) {
			c.PortMaps = []PortMap{{HostPort: 80, GuestPort: 80, Protocol: "icmp"}}
		}, true},
		{"zero host port", func(c *Config) {
			c.PortMaps = []PortMap{{HostPort: 0, GuestPort: 80, Protocol: ProtocolTCP}}
		}, true},
		{"bad ip", func(c *Config) { c.IPAddress = "999.1.1.1" }, true},
		{"bad gateway", func(c *Config) { c.GatewayIP = "nope" }, true},
		{"bad subnet mask", func(c *Config) { c.SubnetMask = "40" }, true},
		{"empty volume path", func(c *Config) {
			c.Volumes = []VolumeMount{{DiskPath: ""}}
		}, true},
		{"valid env", func(c *Config) { c.Env = []string{"FOO=bar", "PORT=8080"} }, false},
		{"env missing equals", func(c *Config) { c.Env = []string{"FOO"} }, true},
		{"env bad key", func(c *Config) { c.Env = []string{"1BAD=x"} }, true},
		{"env value with space (boot-arg injection)", func(c *Config) {
			c.Env = []string{"X=val en1.gateway=10.0.0.66"}
		}, true},
		{"env value with newline", func(c *Config) { c.Env = []string{"X=a\nb"} }, true},
		{"env value with vertical tab", func(c *Config) { c.Env = []string{"X=a\vb"} }, true},
		{"env value with form feed", func(c *Config) { c.Env = []string{"X=a\fb"} }, true},
		{"valid volume label and path", func(c *Config) {
			c.Volumes = []VolumeMount{{DiskPath: "d.img", Label: "pgdata", GuestPath: "/db"}}
		}, false},
		{"volume label with comma", func(c *Config) {
			c.Volumes = []VolumeMount{{DiskPath: "d.img", Label: "a,b", GuestPath: "/db"}}
		}, false},
		{"volume label with equals", func(c *Config) {
			c.Volumes = []VolumeMount{{DiskPath: "d.img", Label: "a=b", GuestPath: "/db"}}
		}, true},
		{"volume label with colon", func(c *Config) {
			c.Volumes = []VolumeMount{{DiskPath: "d.img", Label: "a:b", GuestPath: "/db"}}
		}, true},
		{"volume label with dot", func(c *Config) {
			c.Volumes = []VolumeMount{{DiskPath: "d.img", Label: "a.b", GuestPath: "/db"}}
		}, true},
		{"volume label with space", func(c *Config) {
			c.Volumes = []VolumeMount{{DiskPath: "d.img", Label: "a b", GuestPath: "/db"}}
		}, true},
		{"volume guest path relative", func(c *Config) {
			c.Volumes = []VolumeMount{{DiskPath: "d.img", Label: "l", GuestPath: "db"}}
		}, true},
		{"volume guest path with space", func(c *Config) {
			c.Volumes = []VolumeMount{{DiskPath: "d.img", Label: "l", GuestPath: "/d b"}}
		}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			tt.mutate(&cfg)
			err := validateVMConfig(cfg)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
