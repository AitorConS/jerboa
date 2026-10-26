//go:build darwin && arm64

package vm

import "testing"

func TestQEMUGuestEgressPolicy(t *testing.T) {
	m := NewQEMUManager("qemu")
	for addr, want := range map[string]bool{
		"1.1.1.1":     true,
		"203.0.113.9": true,
		"127.0.0.1":   false,
		"10.0.2.2":    false,
		"192.168.1.1": false,
		"172.20.0.5":  false,
		"169.254.1.1": false,
		"100.64.0.1":  false,
		"224.0.0.1":   false,
		"bogus":       false,
	} {
		if got := m.nativeEgress("", addr, 80, false); got != want {
			t.Errorf("egress to %s = %v, want %v", addr, got, want)
		}
	}
	if NewQEMUManager("qemu", WithGuestHostAccess(true)).nativeEgress != nil {
		t.Fatal("WithGuestHostAccess must disable the egress policy")
	}
}
