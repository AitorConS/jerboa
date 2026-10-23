package image

import (
	"testing"

	"github.com/stretchr/testify/require"

	pkg "github.com/AitorConS/jerboa/internal/package"
)

func TestManifestHosts(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     []string
	}{
		{"plain", `(children:(a:(contents:(host:"/x"))))`, []string{"/x"}},
		{"unquoted", `(children:(a:(contents:(host:/x))))`, []string{"/x"}},
		{"space before colon", "(children:(a:(contents:(host :\"/x\"))))", []string{"/x"}},
		{"tab before colon", "(children:(a:(contents:(host\t:/x))))", []string{"/x"}},
		{"quoted name", `(children:(a:(contents:("host":"/x"))))`, []string{"/x"}},
		{"escaped name", `(children:(a:(contents:("h\ost":"/x"))))`, []string{"/x"}},
		{"comment", "(# host:/y\n children:(a:(contents:(host:\"/x\"))))", []string{"/x"}},
		{"escaped quote in path", `(children:(a:(contents:(host:"/x\"y"))))`, []string{`/x"y`}},
		{"in vector", `(v:[(host:/x) b])`, []string{"/x"}},
		{"none", `(program:/p environment:())`, nil},
		{"later wins", `(a:(host:/x host:/y))`, []string{"/y"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ManifestHosts(tt.manifest)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestParseManifestRejects(t *testing.T) {
	for name, m := range map[string]string{
		"two tuples":   `(a:b)(c:d)`,
		"incomplete":   `(a:(b:c)`,
		"no tuple":     `a:b`,
		"bad sigil":    `(a|b)`,
		"control char": "(a:\x00)",
		"invalid utf8": "(a:\xff)",
		"host tuple":   `(a:(contents:(host:(x:y))))`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ManifestHosts(m)
			require.Error(t, err)
		})
	}
}

func TestParseManifestReadsBuiltManifests(t *testing.T) {
	m := buildManifest(BuildConfig{
		BinaryPath: "/bin/app",
		Args:       []string{"a b", `c"d`},
		Env:        map[string]string{"K": "v w"},
		PkgFiles:   []pkg.File{{GuestPath: "etc/x (1).txt", HostPath: "/pkg/x (1).txt"}},
	}, func(h string) string { return h })
	hosts, err := ManifestHosts(m)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"/pkg/x (1).txt", "/bin/app"}, hosts)
}

func TestVolumeManifestKeepsQuotesInFileNames(t *testing.T) {
	name := `x"))y:(contents:(host:"hosts`
	m := BuildVolumeManifest([]pkg.File{{GuestPath: name, HostPath: "/ctx/" + name}})
	hosts, err := ManifestHosts(m)
	require.NoError(t, err)
	require.Equal(t, []string{"/ctx/" + name}, hosts)
	root, err := ParseManifest(m)
	require.NoError(t, err)
	require.Equal(t, name, root.Tuple[0].Value.Tuple[0].Name)
}
