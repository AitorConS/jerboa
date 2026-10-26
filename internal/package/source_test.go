package pkg

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDownloadRefusesUnverifiableEntries(t *testing.T) {
	good := Package{
		Name:    "p",
		Version: "1",
		Size:    10,
		SHA256:  strings.Repeat("a", 64),
		URL:     "https://example.com/p.tar.gz",
	}
	require.NoError(t, good.checkSource())

	for name, mutate := range map[string]func(*Package){
		"no sha256":  func(p *Package) { p.SHA256 = "" },
		"short hash": func(p *Package) { p.SHA256 = "abc" },
		"no size":    func(p *Package) { p.Size = 0 },
		"plain http": func(p *Package) { p.URL = "http://example.com/p.tar.gz" },
		"file url":   func(p *Package) { p.URL = "file:///etc/passwd" },
		"no host":    func(p *Package) { p.URL = "https:///p.tar.gz" },
		"lookalike":  func(p *Package) { p.URL = "http://127.0.0.1.example.com/p" },
	} {
		t.Run(name, func(t *testing.T) {
			p := good
			mutate(&p)
			require.Error(t, p.checkSource())
			store, err := NewStore(t.TempDir())
			require.NoError(t, err)
			require.Error(t, store.Download(p))
		})
	}

	local := good
	local.URL = "http://127.0.0.1:8080/p.tar.gz"
	require.NoError(t, local.checkSource())
}

func TestCheckIndexRefusesMisfiledEntries(t *testing.T) {
	require.NoError(t, checkIndex(&Index{Packages: map[string][]Package{"node": {{Name: "node"}}}}))
	require.Error(t, checkIndex(&Index{Packages: map[string][]Package{"node": {{Name: "python"}}}}))
}
