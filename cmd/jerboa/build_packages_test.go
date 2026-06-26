package main

import "testing"

func TestSameRuntimeFamily(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"python", "python", true},
		{"python", "python3", true},
		{"python3", "python", true},
		{"node", "node20", true},
		{"node", "node-exporter", false}, // shared prefix, not a version suffix
		{"python3", "python2", false},    // different versions
		{"node", "deno", false},
		{"go", "golang", false},
	}
	for _, tt := range tests {
		if got := sameRuntimeFamily(tt.a, tt.b); got != tt.want {
			t.Errorf("sameRuntimeFamily(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestFilterCoveredAutoPkgsKeepsUnrelated(t *testing.T) {
	auto := []string{"node"}
	user := []string{"node-exporter"}
	got := filterCoveredAutoPkgs(auto, user)
	if len(got) != 1 || got[0] != "node" {
		t.Fatalf("expected node runtime kept, got %v", got)
	}
}
