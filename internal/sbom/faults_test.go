package sbom

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An SBOM is generated from whatever files an image is built from, and those
// are other people's files: truncated, oversized, malformed, or written to
// confuse a reader. These tests hold the generator to this: it writes a
// document that is valid and the same every time, or it fails; a file it
// cannot read is an error, never a silently shorter list of contents.

var purlRE = regexp.MustCompile(`^pkg:[a-z]+/[A-Za-z0-9._~%/-]+(@[A-Za-z0-9._~%-]+)?(\?platform=[A-Za-z0-9._~%-]+)?$`)

// requireSound checks a generated document: valid JSON, accepted by Check,
// with well-formed, unique package URLs.
func requireSound(t testing.TB, data []byte) BOM {
	t.Helper()
	require.True(t, json.Valid(data), "the document is not JSON")
	require.NoError(t, Check(data))
	var bom BOM
	require.NoError(t, json.Unmarshal(data, &bom))
	require.Regexp(t, `^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-8[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, bom.SerialNumber)
	seen := map[string]bool{}
	for _, c := range bom.Components {
		require.Regexp(t, purlRE, c.PURL, "component %q", c.Name)
		require.False(t, seen[c.BOMRef], "component %s is listed twice", c.BOMRef)
		seen[c.BOMRef] = true
		for _, l := range c.Licenses {
			require.NotContains(t, l.License.Name, "\n")
			require.LessOrEqual(t, len(l.License.Name), 100)
		}
	}
	return bom
}

func names(bom BOM) []string {
	out := make([]string, len(bom.Components))
	for i, c := range bom.Components {
		out[i] = c.Name + "@" + c.Version
	}
	return out
}

// Module manifests that are damaged are skipped, not guessed at, and never
// stop the modules beside them from being listed.
func TestFaults_DamagedModuleManifests(t *testing.T) {
	whole := `{"name":"express","version":"4.19.2","license":"MIT"}`
	damaged := map[string]string{
		"empty":              "",
		"truncated":          whole[:len(whole)/2],
		"zeroed":             strings.Repeat("\x00", len(whole)),
		"an array":           `[]`,
		"null":               `null`,
		"name of wrong type": `{"name":["express"],"version":"4.19.2"}`,
		"no version":         `{"name":"express"}`,
		"no name":            `{"version":"4.19.2"}`,
		"binary":             "\x7fELF\x02\x01\x01",
	}
	for name, content := range damaged {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			files := []pkg.File{
				write(t, dir, "node_modules/express/package.json", content),
				write(t, dir, "node_modules/debug/package.json", `{"name":"debug","version":"2.6.9"}`),
			}
			data, err := Generate(Input{Platform: "linux/amd64", Files: files})
			require.NoError(t, err)
			bom := requireSound(t, data)
			assert.Equal(t, []string{"debug@2.6.9"}, names(bom))
		})
	}
}

// The license field is free text in the wild: a whole license file, an
// object, a list. Only a short one-line name is recorded.
func TestFaults_LicenseFieldsThatAreNotNames(t *testing.T) {
	longText := strings.Repeat("Permission is hereby granted, free of charge, ", 10)
	cases := map[string]string{
		`"MIT"`:                        "MIT",
		`{"type":"ISC","url":"x"}`:     "ISC",
		`["MIT","Apache-2.0"]`:         "",
		`42`:                           "",
		`null`:                         "",
		`"UNKNOWN"`:                    "",
		`"MIT\nCopyright (c) someone"`: "",
		`"` + longText + `"`:           "",
		`"  "`:                         "",
		`{"type":{"nested":"object"}}`: "",
	}
	for license, want := range cases {
		dir := t.TempDir()
		files := []pkg.File{write(t, dir, "node_modules/m/package.json", `{"name":"m","version":"1.0.0","license":`+license+`}`)}
		data, err := Generate(Input{Platform: "linux/amd64", Files: files})
		require.NoError(t, err, license)
		bom := requireSound(t, data)
		require.Len(t, bom.Components, 1, license)
		if want == "" {
			assert.Empty(t, bom.Components[0].Licenses, license)
		} else {
			require.Len(t, bom.Components[0].Licenses, 1, license)
			assert.Equal(t, want, bom.Components[0].Licenses[0].License.Name)
		}
	}
}

// Python metadata cut short or malformed: what is named is listed, what is
// not is left out, and the description after the headers is never read as
// headers.
func TestFaults_DamagedPythonMetadata(t *testing.T) {
	cases := map[string][]string{
		"Metadata-Version: 2.1\nName: requests\nVersion: 2.31.0\n":  {"requests@2.31.0"},
		"Metadata-Version: 2.1\nName: requests\nVers":               {},
		"Metadata-Version: 2.1\nName: requests\n\nVersion: 9.9.9\n": {},
		"":                             {},
		"\n\nName: late\nVersion: 1\n": {},
		"Name: a\nName: b\nVersion: 1\nVersion: 2\n":              {"a@1"},
		"Name: folded\n Version: 6.6.6\nVersion: 1.0\n":           {"folded@1.0"},
		"Name:   spaced_Name.x  \nVersion:\t3 \n":                 {"spaced_Name.x@3"},
		"Name: crlf\r\nVersion: 1.2\r\n\r\nbody\r\n":              {"crlf@1.2"},
		"no colon here\nName: ok\nVersion: 1\n":                   {"ok@1"},
		"Name: " + strings.Repeat("n", 70<<10) + "\nVersion: 1\n": {strings.Repeat("n", 70<<10) + "@1"},
		"\x00\x01\x02\xff\xfe":                                    {},
	}
	for content, want := range cases {
		dir := t.TempDir()
		files := []pkg.File{write(t, dir, "lib/python3.12/site-packages/x-1.dist-info/METADATA", content)}
		data, err := Generate(Input{Platform: "linux/amd64", Files: files})
		require.NoError(t, err, "%q", content[:min(len(content), 40)])
		bom := requireSound(t, data)
		assert.Equal(t, want, names(bom), "%q", content[:min(len(content), 40)])
	}
}

// A header line longer than the scanner will take ends the reading of that
// file's headers; it must not panic or hang, and the document is still valid.
func TestFaults_PythonMetadataWithAnEndlessLine(t *testing.T) {
	dir := t.TempDir()
	files := []pkg.File{
		write(t, dir, "site-packages/big-1.dist-info/METADATA", "Name: big\nX: "+strings.Repeat("x", 2<<20)+"\nVersion: 1\n"),
		write(t, dir, "site-packages/ok-1.dist-info/METADATA", "Name: ok\nVersion: 1\n"),
	}
	data, err := Generate(Input{Platform: "linux/amd64", Files: files})
	require.NoError(t, err)
	bom := requireSound(t, data)
	assert.Contains(t, names(bom), "ok@1")
	assert.NotContains(t, names(bom), "big@1", "a version read past an unreadable line")
}

// A manifest far larger than a manifest is read up to a bound.
func TestFaults_OversizedManifestIsBounded(t *testing.T) {
	dir := t.TempDir()
	huge := `{"name":"huge","version":"1.0.0","padding":"` + strings.Repeat("x", maxManifest) + `"}`
	files := []pkg.File{
		write(t, dir, "node_modules/huge/package.json", huge),
		write(t, dir, "node_modules/ok/package.json", `{"name":"ok","version":"1.0.0"}`),
	}
	data, err := Generate(Input{Platform: "linux/amd64", Files: files})
	require.NoError(t, err)
	bom := requireSound(t, data)
	assert.Equal(t, []string{"ok@1.0.0"}, names(bom), "a manifest cut at the bound was read as a module")
	assert.Less(t, len(data), 4096)
}

// Files that cannot be read are errors: an SBOM that left them out would
// claim the image holds less than it does.
func TestFaults_UnreadableInputsAreErrors(t *testing.T) {
	dir := t.TempDir()
	ok := write(t, dir, "node_modules/ok/package.json", `{"name":"ok","version":"1.0.0"}`)
	gone := pkg.File{HostPath: filepath.Join(dir, "node_modules", "gone", "package.json"), GuestPath: "node_modules/gone/package.json"}
	goneMeta := pkg.File{HostPath: filepath.Join(dir, "x.dist-info", "METADATA"), GuestPath: "site-packages/x.dist-info/METADATA"}

	_, err := Generate(Input{Platform: "linux/amd64", Files: []pkg.File{ok, gone}})
	require.Error(t, err)
	_, err = Generate(Input{Platform: "linux/amd64", Files: []pkg.File{goneMeta, ok}})
	require.Error(t, err)
	_, err = Generate(Input{Platform: "linux/amd64", Program: filepath.Join(dir, "no-such-program")})
	require.Error(t, err)
	_, err = Generate(Input{Platform: "linux/amd64", Program: dir})
	require.Error(t, err, "a directory is not a program")

	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		secret := write(t, dir, "node_modules/secret/package.json", `{"name":"secret","version":"1.0.0"}`)
		require.NoError(t, os.Chmod(secret.HostPath, 0o000))
		_, err = Generate(Input{Platform: "linux/amd64", Files: []pkg.File{ok, secret}})
		require.Error(t, err)

		src := t.TempDir()
		write(t, src, "Cargo.toml", "[package]\nname = \"app\"\n")
		lock := write(t, src, "Cargo.lock", "version = 3\n")
		require.NoError(t, os.Chmod(lock.HostPath, 0o000))
		_, err = Generate(Input{Platform: "linux/amd64", SourceDir: src})
		require.Error(t, err, "an unreadable Cargo.lock was treated as no dependencies")
	}

	// Directories and entries without a host file are not read at all.
	data, err := Generate(Input{Platform: "linux/amd64", Files: []pkg.File{{GuestPath: "node_modules/d/package.json", IsDir: true, HostPath: dir}, {GuestPath: "node_modules/e/package.json"}, ok}})
	require.NoError(t, err)
	assert.Equal(t, []string{"ok@1.0.0"}, names(requireSound(t, data)))
}

// A Cargo.lock cut short or mangled fails the SBOM. Listing the crates that
// happened to parse would publish a signed document missing dependencies.
func TestFaults_DamagedCargoLock(t *testing.T) {
	whole := "version = 3\n\n[[package]]\nname = \"serde\"\nversion = \"1.0.200\"\nsource = \"registry+https://github.com/rust-lang/crates.io-index\"\nchecksum = \"" + strings.Repeat("a", 64) + "\"\n\n[[package]]\nname = \"tokio\"\nversion = \"1.37.0\"\nsource = \"registry+https://github.com/rust-lang/crates.io-index\"\n"
	generate := func(lock string) ([]byte, error) {
		src := t.TempDir()
		write(t, src, "Cargo.toml", "[package]\nname = \"app\"\n")
		write(t, src, "Cargo.lock", lock)
		return Generate(Input{Platform: "linux/amd64", SourceDir: src})
	}
	data, err := generate(whole)
	require.NoError(t, err)
	require.Equal(t, []string{"serde@1.0.200", "tokio@1.37.0"}, names(requireSound(t, data)))

	for name, lock := range map[string]string{
		"cut in a string":     whole[:strings.Index(whole, "1.0.200")+3],
		"cut in a header":     whole[:strings.LastIndex(whole, "[[package]]")+5],
		"zeroed":              strings.Repeat("\x00", 64),
		"binary":              "\xff\xfe\x00\x01",
		"package is a string": "package = \"serde\"\n",
		"json":                `{"package":[{"name":"serde"}]}`,
	} {
		_, err := generate(lock)
		require.Error(t, err, name)
	}

	// Entries that name no registry crate are the project's own and skipped;
	// a checksum that is not one is left out rather than recorded.
	data, err = generate("[[package]]\nname = \"app\"\nversion = \"0.1.0\"\n\n[[package]]\nname = \"dep\"\nversion = \"1.0.0\"\nsource = \"git+https://example.com/dep\"\nchecksum = \"not-a-checksum\"\n\n[[package]]\nname = \"\"\nversion = \"1\"\nsource = \"x\"\n")
	require.NoError(t, err)
	bom := requireSound(t, data)
	require.Equal(t, []string{"dep@1.0.0"}, names(bom))
	assert.Empty(t, bom.Components[0].Hashes)

	// No Cargo.toml: the directory is not a Rust project, whatever else it holds.
	src := t.TempDir()
	write(t, src, "Cargo.lock", "this is not toml [[[")
	_, err = Generate(Input{Platform: "linux/amd64", SourceDir: src})
	require.NoError(t, err)
	_, err = Generate(Input{Platform: "linux/amd64", SourceDir: filepath.Join(src, "absent")})
	require.NoError(t, err)
}

// Files that start like an ELF binary and are not one (a truncated download,
// a damaged build output) are not Go binaries, and are not errors.
func TestFaults_BinariesThatAreNotWhole(t *testing.T) {
	dir := t.TempDir()
	self, err := os.ReadFile(os.Args[0])
	require.NoError(t, err)
	cases := map[string][]byte{
		"magic only":                  []byte("\x7fELF"),
		"short header":                append([]byte("\x7fELF\x02\x01\x01"), make([]byte, 20)...),
		"zeroed header":               append([]byte("\x7fELF"), make([]byte, 4096)...),
		"three bytes":                 []byte("\x7fEL"),
		"empty":                       nil,
		"text":                        []byte("#!/bin/sh\necho hi\n"),
		"this test binary, truncated": self[:min(len(self), 64<<10)],
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
			require.NoError(t, os.WriteFile(p, content, 0o755))
			data, err := Generate(Input{Platform: "linux/amd64", Program: p, Files: []pkg.File{{HostPath: p, GuestPath: "bin/tool"}}})
			require.NoError(t, err)
			bom := requireSound(t, data)
			require.Len(t, bom.Metadata.Component.Hashes, 1, "the program is identified by its digest even when nothing is known of it")
		})
	}
}

// Names from manifests go into package URLs and JSON. Whatever they hold,
// the document stays valid and each name is one component, not several.
func TestFaults_HostileNamesStayInsideTheirFields(t *testing.T) {
	hostile := []string{
		`"},{"type":"library","name":"injected`,
		"../../etc/passwd",
		"name with spaces",
		"@scope/name",
		"@scope/../../x",
		"a?b=c#d",
		"pkg:npm/other@9.9.9",
		"na\u0000me",
		"ñandú/日本語",
		strings.Repeat("a/", 300) + "b",
		"%2e%2e%2f",
		"\u202egnp.exe",
	}
	dir := t.TempDir()
	var files []pkg.File
	for i, name := range hostile {
		manifest, err := json.Marshal(map[string]string{"name": name, "version": "1.0.0+" + name})
		require.NoError(t, err)
		files = append(files, write(t, dir, "node_modules/m"+string(rune('a'+i))+"/package.json", string(manifest)))
	}
	refs := []pkg.Reference{{Name: `evil"name`, Version: "1 2", Platform: "linux/amd64&x=y", SHA256: "sha256:not-hex", Source: "jerboa"}}
	data, err := Generate(Input{Platform: "linux/amd64", Files: files, Packages: refs})
	require.NoError(t, err)
	bom := requireSound(t, data)
	require.Len(t, bom.Components, len(hostile)+1)
	for _, c := range bom.Components {
		assert.NotEqual(t, "injected", c.Name)
		assert.NotContains(t, c.PURL, " ")
		if c.Name == `evil"name` {
			assert.Empty(t, c.Hashes, "a digest that is not one was recorded")
			assert.Contains(t, c.PURL, "?platform=linux%2Famd64%26x%3Dy")
		}
	}
}

// The same inputs give the same bytes, in whatever order the files come:
// the SBOM is signed and compared by digest.
func TestFaults_GenerateIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	files := []pkg.File{
		write(t, dir, "node_modules/b/package.json", `{"name":"b","version":"2.0.0"}`),
		write(t, dir, "node_modules/a/package.json", `{"name":"a","version":"1.0.0","license":"MIT"}`),
		write(t, dir, "site-packages/c-3.dist-info/METADATA", "Name: c\nVersion: 3\n"),
		write(t, dir, "node_modules/x/node_modules/a/package.json", `{"name":"a","version":"1.0.0"}`),
	}
	in := Input{Platform: "linux/arm64", Files: files, Tool: "v1.2.3", Created: "2026-01-02T03:04:05Z",
		Packages: []pkg.Reference{{Name: "node", Version: "20.1.0"}, {Name: "ca-certificates", Version: "1"}}}
	first, err := Generate(in)
	require.NoError(t, err)
	requireSound(t, first)
	for range 3 {
		again, err := Generate(in)
		require.NoError(t, err)
		require.True(t, bytes.Equal(first, again))
	}
	reversed := in
	reversed.Packages = []pkg.Reference{in.Packages[1], in.Packages[0]}
	reversed.Files = []pkg.File{files[2], files[1], files[0], files[3]}
	other, err := Generate(reversed)
	require.NoError(t, err)
	assert.Equal(t, names(requireSound(t, first)), names(requireSound(t, other)))

	// A different image is a different document, with a different serial.
	changed := in
	changed.Files = files[:2]
	third, err := Generate(changed)
	require.NoError(t, err)
	assert.NotEqual(t, requireSound(t, first).SerialNumber, requireSound(t, third).SerialNumber)
}

// Check is what the daemon runs on an SBOM a client hands it.
func TestFaults_CheckRefusesWhatIsNotAnSBOM(t *testing.T) {
	whole, err := Generate(Input{Platform: "linux/amd64"})
	require.NoError(t, err)
	require.NoError(t, Check(whole))
	for n := range len(whole) {
		require.Error(t, Check(whole[:n]), "the first %d bytes passed", n)
	}
	for name, data := range map[string][]byte{
		"empty":                nil,
		"null":                 []byte("null"),
		"an array":             []byte("[]"),
		"a string":             []byte(`"CycloneDX"`),
		"spdx":                 []byte(`{"spdxVersion":"SPDX-2.3"}`),
		"no spec version":      []byte(`{"bomFormat":"CycloneDX"}`),
		"format lower-case":    []byte(`{"bomFormat":"cyclonedx","specVersion":"1.6"}`),
		"format of wrong type": []byte(`{"bomFormat":["CycloneDX"],"specVersion":"1.6"}`),
		"two documents":        append(bytes.Clone(whole), whole...),
		"over the limit":       append(bytes.Clone(whole[:len(whole)-1]), append(bytes.Repeat([]byte(" "), MaxSize), '}')...),
	} {
		require.Error(t, Check(data), name)
	}
	padded := append(bytes.Clone(whole[:len(whole)-1]), append(bytes.Repeat([]byte(" "), MaxSize-len(whole)), '}')...)
	require.Len(t, padded, MaxSize)
	require.NoError(t, Check(padded), "a document of exactly the limit is allowed")
}

func FuzzGenerateFromManifests(f *testing.F) {
	f.Add(`{"name":"express","version":"4.19.2","license":"MIT"}`, "Name: requests\nVersion: 2.31.0\nLicense: Apache-2.0\n", "[[package]]\nname = \"serde\"\nversion = \"1\"\nsource = \"registry\"\n")
	f.Add(`{"name":"@a/b","version":"1","license":{"type":"ISC"}}`, "Name: a_b.c\nVersion: 1\n\nName: x\n", "")
	f.Add(`{"name":"\"},{","version":"\n"}`, "Name:\nVersion:\n", "[[package]]\nname = 1\n")
	f.Add(``, ``, `[[package`)
	f.Fuzz(func(t *testing.T, npm, python, cargo string) {
		dir := t.TempDir()
		src := t.TempDir()
		files := []pkg.File{
			write(t, dir, "node_modules/m/package.json", npm),
			write(t, dir, "site-packages/d-1.dist-info/METADATA", python),
		}
		write(t, src, "Cargo.toml", "[package]\n")
		write(t, src, "Cargo.lock", cargo)
		in := Input{Platform: "linux/amd64", Files: files, SourceDir: src}
		data, err := Generate(in)
		if err != nil {
			return
		}
		requireSound(t, data)
		again, err := Generate(in)
		require.NoError(t, err)
		require.True(t, bytes.Equal(data, again), "the same inputs gave two documents")
	})
}

func FuzzCheck(f *testing.F) {
	whole, err := Generate(Input{Platform: "linux/amd64"})
	require.NoError(f, err)
	f.Add(whole)
	f.Add(whole[:len(whole)/2])
	f.Add([]byte(`{"bomFormat":"CycloneDX","specVersion":"1.6"}`))
	f.Add([]byte(`{"bomFormat":"CycloneDX","specVersion":""}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if Check(data) != nil {
			return
		}
		require.True(t, json.Valid(data))
		require.LessOrEqual(t, len(data), MaxSize)
		var head map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(data, &head))
	})
}
