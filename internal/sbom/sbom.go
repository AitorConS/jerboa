// Package sbom describes what an image contains as a CycloneDX 1.6 software
// bill of materials: the packages it was built with, the Go modules compiled
// into its binaries, and the npm, Python and Rust dependencies it ships.
//
// The document depends only on its inputs, so building the same image twice
// gives the same bytes: components are sorted, the serial number is derived
// from the content, and the timestamp is the build's SOURCE_DATE_EPOCH (or
// absent).
package sbom

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/pelletier/go-toml/v2"
)

// MediaType identifies a CycloneDX JSON document.
const MediaType = "application/vnd.cyclonedx+json"

// SpecVersion is the CycloneDX version this package writes.
const SpecVersion = "1.6"

// MaxSize bounds an SBOM the daemon stores with an image.
const MaxSize = 8 << 20

// BOM is a CycloneDX document, reduced to the fields this package writes.
type BOM struct {
	Schema       string      `json:"$schema"`
	BOMFormat    string      `json:"bomFormat"`
	SpecVersion  string      `json:"specVersion"`
	SerialNumber string      `json:"serialNumber"`
	Version      int         `json:"version"`
	Metadata     Metadata    `json:"metadata"`
	Components   []Component `json:"components"`
}

// Metadata names the image's program as the document's subject.
type Metadata struct {
	Timestamp  string     `json:"timestamp,omitempty"`
	Tools      *Tools     `json:"tools,omitempty"`
	Component  Component  `json:"component"`
	Properties []Property `json:"properties,omitempty"`
}

// Tools lists what wrote the document.
type Tools struct {
	Components []Component `json:"components"`
}

// Component is one package, module or library in the image.
type Component struct {
	Type       string     `json:"type"`
	BOMRef     string     `json:"bom-ref,omitempty"`
	Name       string     `json:"name"`
	Version    string     `json:"version,omitempty"`
	PURL       string     `json:"purl,omitempty"`
	Hashes     []Hash     `json:"hashes,omitempty"`
	Licenses   []License  `json:"licenses,omitempty"`
	Properties []Property `json:"properties,omitempty"`
}

// Hash is a digest of a component's content.
type Hash struct {
	Alg     string `json:"alg"`
	Content string `json:"content"`
}

// License is a license as its package declares it.
type License struct {
	License LicenseName `json:"license"`
}

// LicenseName is a license given by name rather than as an SPDX expression,
// since packages do not always declare a valid one.
type LicenseName struct {
	Name string `json:"name"`
}

// Property is a name and value CycloneDX does not define a field for.
type Property struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Input is what an image is built from.
type Input struct {
	// Platform is the image's platform, such as linux/arm64.
	Platform string
	// Program is the host path of the program the image runs, and
	// ProgramPath its path in the image (empty for /program).
	Program     string
	ProgramPath string
	// Files are the files that enter the image.
	Files []pkg.File
	// Packages are the packages the image was built with.
	Packages []pkg.Reference
	// SourceDir is the project directory, for its Cargo.lock; empty when
	// the image was built from a binary.
	SourceDir string
	// Tool is the version of jerboa writing the document.
	Tool string
	// Created is the build time (RFC 3339); empty leaves the timestamp out.
	Created string
}

// Generate returns the SBOM of an image, as compact JSON.
func Generate(in Input) ([]byte, error) {
	program, err := programComponent(in)
	if err != nil {
		return nil, err
	}
	c := collector{seen: map[string]bool{}}
	for _, r := range in.Packages {
		c.add(packageComponent(r))
	}
	if err := c.goModules(in.Program); err != nil {
		return nil, err
	}
	for _, f := range in.Files {
		if f.IsDir || f.HostPath == "" {
			continue
		}
		if err := c.file(f); err != nil {
			return nil, err
		}
	}
	if err := c.cargo(in.SourceDir); err != nil {
		return nil, err
	}
	sort.Slice(c.components, func(i, j int) bool { return c.components[i].BOMRef < c.components[j].BOMRef })

	bom := BOM{
		Schema:      "http://cyclonedx.org/schema/bom-1.6.schema.json",
		BOMFormat:   "CycloneDX",
		SpecVersion: SpecVersion,
		Version:     1,
		Metadata: Metadata{
			Timestamp:  in.Created,
			Tools:      &Tools{Components: []Component{{Type: "application", Name: "jerboa", Version: in.Tool}}},
			Component:  program,
			Properties: []Property{{Name: "jerboa:platform", Value: in.Platform}},
		},
		Components: c.components,
	}
	if bom.Components == nil {
		bom.Components = []Component{}
	}
	data, err := json.Marshal(bom)
	if err != nil {
		return nil, fmt.Errorf("sbom: encode: %w", err)
	}
	bom.SerialNumber = serialNumber(data)
	data, err = json.Marshal(bom)
	if err != nil {
		return nil, fmt.Errorf("sbom: encode: %w", err)
	}
	return data, nil
}

// Check reports whether data is a CycloneDX JSON document small enough to
// store with an image.
func Check(data []byte) error {
	if len(data) > MaxSize {
		return fmt.Errorf("sbom: %d bytes is over the %d byte limit", len(data), MaxSize)
	}
	var head struct {
		BOMFormat   string `json:"bomFormat"`
		SpecVersion string `json:"specVersion"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return fmt.Errorf("sbom: not JSON: %w", err)
	}
	if head.BOMFormat != "CycloneDX" || head.SpecVersion == "" {
		return errors.New("sbom: not a CycloneDX document")
	}
	return nil
}

// serialNumber derives the document's URN from its content, as a version 8
// (custom) UUID, so the same image gets the same document.
func serialNumber(data []byte) string {
	sum := sha256.Sum256(data)
	u := sum[:16]
	u[6] = u[6]&0x0f | 0x80
	u[8] = u[8]&0x3f | 0x80
	h := hex.EncodeToString(u)
	return "urn:uuid:" + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// programComponent is the document's subject: the program the image runs.
func programComponent(in Input) (Component, error) {
	name := path.Base(filepath.ToSlash(in.Program))
	if in.ProgramPath != "" {
		name = path.Base(in.ProgramPath)
	}
	c := Component{Type: "application", BOMRef: "program", Name: name}
	if in.Program == "" {
		return c, nil
	}
	sum, err := fileSHA256(in.Program)
	if err != nil {
		return Component{}, err
	}
	c.Hashes = []Hash{{Alg: "SHA-256", Content: sum}}
	return c, nil
}

// packageComponent describes a package the image was built with.
func packageComponent(r pkg.Reference) Component {
	c := Component{
		Type:    "application",
		Name:    r.Name,
		Version: r.Version,
		PURL:    purl("generic", r.Name, r.Version),
	}
	if r.Platform != "" {
		c.PURL += "?platform=" + escape(r.Platform)
	}
	if sum := strings.TrimPrefix(r.SHA256, "sha256:"); isHex64(sum) {
		c.Hashes = []Hash{{Alg: "SHA-256", Content: sum}}
	}
	if r.Source != "" {
		c.Properties = []Property{{Name: "jerboa:package-source", Value: r.Source}}
	}
	return c
}

type collector struct {
	components []Component
	seen       map[string]bool
}

// add records c once, keyed by its package URL.
func (c *collector) add(comp Component) {
	if comp.BOMRef == "" {
		comp.BOMRef = comp.PURL
	}
	if c.seen[comp.BOMRef] {
		return
	}
	c.seen[comp.BOMRef] = true
	c.components = append(c.components, comp)
}

// file looks at one image file for what it says about the image's contents:
// an npm module's package.json, a Python distribution's metadata, or a Go
// binary's module list.
func (c *collector) file(f pkg.File) error {
	segs := strings.Split(strings.Trim(filepath.ToSlash(f.GuestPath), "/"), "/")
	n := len(segs)
	switch {
	case n >= 3 && segs[n-1] == "package.json" &&
		(segs[n-3] == "node_modules" || (n >= 4 && segs[n-4] == "node_modules" && strings.HasPrefix(segs[n-3], "@"))):
		return c.npm(f.HostPath)
	case n >= 2 && (segs[n-1] == "METADATA" && strings.HasSuffix(segs[n-2], ".dist-info") ||
		segs[n-1] == "PKG-INFO" && strings.HasSuffix(segs[n-2], ".egg-info")):
		return c.python(f.HostPath)
	case f.Reference == nil:
		// The project's own files and copied stage artifacts; package files
		// are described by their package.
		return c.goModules(f.HostPath)
	}
	return nil
}

// goModules adds the Go toolchain and modules compiled into the binary at
// hostPath, if it is a Go binary.
func (c *collector) goModules(hostPath string) error {
	if hostPath == "" || !isELF(hostPath) {
		return nil
	}
	bi, err := buildinfo.ReadFile(hostPath)
	if err != nil {
		// Not a Go binary.
		return nil //nolint:nilerr // only Go binaries carry build info
	}
	if v := strings.TrimPrefix(bi.GoVersion, "go"); v != "" {
		c.add(Component{Type: "library", Name: "stdlib", Version: v, PURL: purl("golang", "stdlib", v)})
	}
	for _, d := range bi.Deps {
		if d.Replace != nil {
			d = d.Replace
		}
		if d.Path == "" || strings.HasPrefix(d.Path, ".") || strings.HasPrefix(d.Path, "/") {
			continue
		}
		version := d.Version
		if version == "(devel)" {
			version = ""
		}
		c.add(Component{Type: "library", Name: d.Path, Version: version, PURL: purl("golang", d.Path, version)})
	}
	return nil
}

// npm adds the module whose package.json is at hostPath.
func (c *collector) npm(hostPath string) error {
	data, err := readSmall(hostPath)
	if err != nil {
		return err
	}
	var p struct {
		Name    string          `json:"name"`
		Version string          `json:"version"`
		License json.RawMessage `json:"license"`
	}
	if json.Unmarshal(data, &p) != nil || p.Name == "" || p.Version == "" {
		// Not a module manifest (some modules ship other package.json files).
		return nil
	}
	comp := Component{Type: "library", Name: p.Name, Version: p.Version, PURL: purl("npm", p.Name, p.Version)}
	var license string
	if json.Unmarshal(p.License, &license) != nil {
		var obj struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(p.License, &obj)
		license = obj.Type
	}
	comp.Licenses = licenses(license)
	c.add(comp)
	return nil
}

// pep503 normalizes a Python distribution name.
var pep503 = regexp.MustCompile(`[-_.]+`)

// python adds the distribution whose metadata is at hostPath.
func (c *collector) python(hostPath string) error {
	data, err := readSmall(hostPath)
	if err != nil {
		return err
	}
	headers := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break // the description follows the headers
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		if _, dup := headers[k]; !dup {
			headers[k] = strings.TrimSpace(v)
		}
	}
	name, version := headers["Name"], headers["Version"]
	if name == "" || version == "" {
		return nil
	}
	normalized := strings.ToLower(pep503.ReplaceAllString(name, "-"))
	comp := Component{Type: "library", Name: name, Version: version, PURL: purl("pypi", normalized, version)}
	license := headers["License-Expression"]
	if license == "" {
		license = headers["License"]
	}
	comp.Licenses = licenses(license)
	c.add(comp)
	return nil
}

// cargo adds the crates a Rust project's Cargo.lock pins. The project's own
// crates (those without a source) are the program itself and are left out.
func (c *collector) cargo(dir string) error {
	if dir == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, "Cargo.toml")); err != nil {
		return nil //nolint:nilerr // not a Rust project
	}
	data, err := os.ReadFile(filepath.Join(dir, "Cargo.lock"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sbom: %w", err)
	}
	var lock struct {
		Package []struct {
			Name     string `toml:"name"`
			Version  string `toml:"version"`
			Source   string `toml:"source"`
			Checksum string `toml:"checksum"`
		} `toml:"package"`
	}
	if err := toml.Unmarshal(data, &lock); err != nil {
		return fmt.Errorf("sbom: parse Cargo.lock: %w", err)
	}
	for _, p := range lock.Package {
		if p.Source == "" || p.Name == "" || p.Version == "" {
			continue
		}
		comp := Component{Type: "library", Name: p.Name, Version: p.Version, PURL: purl("cargo", p.Name, p.Version)}
		if isHex64(p.Checksum) {
			comp.Hashes = []Hash{{Alg: "SHA-256", Content: p.Checksum}}
		}
		c.add(comp)
	}
	return nil
}

// licenses records a declared license, if it is a short one-line name
// rather than a license text.
func licenses(name string) []License {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 || strings.ContainsAny(name, "\r\n") || strings.EqualFold(name, "UNKNOWN") {
		return nil
	}
	return []License{{License: LicenseName{Name: name}}}
}

// purl returns the package URL of a package of type typ. Each segment of
// name is percent-encoded; an npm scope's "@" becomes "%40".
func purl(typ, name, version string) string {
	segs := strings.Split(name, "/")
	for i, s := range segs {
		segs[i] = escape(s)
	}
	p := "pkg:" + typ + "/" + strings.Join(segs, "/")
	if version != "" {
		p += "@" + escape(version)
	}
	return p
}

// escape percent-encodes everything but unreserved characters.
func escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if 'a' <= ch && ch <= 'z' || 'A' <= ch && ch <= 'Z' || '0' <= ch && ch <= '9' || strings.IndexByte("-._~", ch) >= 0 {
			b.WriteByte(ch)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", ch)
	}
	return b.String()
}

// isELF reports whether the file at p starts with the ELF magic.
func isELF(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	_, err = io.ReadFull(f, magic[:])
	return err == nil && string(magic[:]) == "\x7fELF"
}

// maxManifest bounds the package.json and metadata files read.
const maxManifest = 4 << 20

func readSmall(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("sbom: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxManifest))
	if err != nil {
		return nil, fmt.Errorf("sbom: read %s: %w", p, err)
	}
	return data, nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("sbom: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("sbom: hash %s: %w", p, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}
