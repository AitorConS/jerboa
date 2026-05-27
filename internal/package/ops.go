package pkg

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/AitorConS/unikernel-engine/internal/httpclient"
)

// OpsPackageBaseURL is the base URL for downloading ops packages.
var OpsPackageBaseURL = "https://repo.ops.city/v2/packages"

// OpsPackageManifestURL stores info about all ops packages.
var OpsPackageManifestURL = "https://repo.ops.city/v2/manifest.json"

// OpsPkghubBaseURL is the base URL of the ops package hub.
var OpsPkghubBaseURL = "https://repo.ops.city"

// OpsPackage mirrors the ops (nanovms/ops) Package struct from lepton/package.go.
type OpsPackage struct {
	Version     string `json:"version"`
	Language    string `json:"language"`
	Description string `json:"description,omitempty"`
	SHA256      string `json:"sha256"`
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Arch        string `json:"arch"`
}

// OpsPackageList mirrors the ops PackageList struct — flat array with schema version.
type OpsPackageList struct {
	Version  int          `json:"Version"`
	Packages []OpsPackage `json:"Packages"`
}

// OpsPackageIdentifier represents a parsed ops package identifier
// in the format <namespace>/<name>:<version>.
type OpsPackageIdentifier struct {
	Namespace string
	Name      string
	Version   string
}

// ParseOpsIdentifier parses an ops package identifier of the form
// "<namespace>/<name>:<version>" or "<namespace>/<name>".
func ParseOpsIdentifier(identifier string) (OpsPackageIdentifier, error) {
	lastSlash := strings.LastIndex(identifier, "/")
	if lastSlash < 1 {
		return OpsPackageIdentifier{}, fmt.Errorf("ops package identifier must include namespace: <namespace>/<name>[:version]")
	}
	namespace := identifier[:lastSlash]
	pkgTokens := strings.SplitN(identifier[lastSlash+1:], ":", 2)
	name := pkgTokens[0]
	version := "latest"
	if len(pkgTokens) > 1 {
		version = pkgTokens[1]
	}
	return OpsPackageIdentifier{
		Namespace: namespace,
		Name:      name,
		Version:   version,
	}, nil
}

// String returns the canonical identifier string.
func (id OpsPackageIdentifier) String() string {
	if id.Version != "" && id.Version != "latest" {
		return fmt.Sprintf("%s/%s:%s", id.Namespace, id.Name, id.Version)
	}
	return fmt.Sprintf("%s/%s", id.Namespace, id.Name)
}

// FetchOpsManifest downloads and parses the ops package manifest.
func FetchOpsManifest() (*OpsPackageList, error) {
	req, err := http.NewRequest(http.MethodGet, OpsPackageManifestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("ops manifest request: %w", err)
	}
	resp, err := httpclient.Default.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ops manifest fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ops manifest: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ops manifest read: %w", err)
	}

	var list OpsPackageList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("ops manifest parse: %w", err)
	}
	return &list, nil
}

// Search returns packages from the manifest matching the query.
func (l *OpsPackageList) Search(query string) []OpsPackage {
	var result []OpsPackage
	lower := strings.ToLower(query)
	for _, pkg := range l.Packages {
		if strings.Contains(strings.ToLower(pkg.Name), lower) ||
			strings.Contains(strings.ToLower(pkg.Description), lower) ||
			strings.Contains(strings.ToLower(pkg.Language), lower) ||
			strings.Contains(strings.ToLower(pkg.Namespace), lower) {
			result = append(result, pkg)
		}
	}
	return result
}

// SearchOpsPackages performs a server-side search against the ops package hub.
func SearchOpsPackages(query string) (*OpsPackageList, error) {
	pkghub, err := url.Parse(OpsPkghubBaseURL + "/api/v1/search")
	if err != nil {
		return nil, fmt.Errorf("ops search url: %w", err)
	}
	q := pkghub.Query()
	q.Add("q", query)
	pkghub.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodGet, pkghub.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("ops search request: %w", err)
	}

	resp, err := httpclient.Default.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ops search: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var list OpsPackageList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("ops search decode: %w", err)
	}
	return &list, nil
}

// Lookup finds a package in the manifest by namespace, name, and optional version.
// If version is empty or "latest", returns the first match for namespace+name.
func (l *OpsPackageList) Lookup(namespace, name, version string) *OpsPackage {
	for i := range l.Packages {
		p := &l.Packages[i]
		if p.Namespace == namespace && p.Name == name {
			if version == "" || version == "latest" || p.Version == version {
				return p
			}
		}
	}
	return nil
}

// ArchSlug returns the architecture suffix used by ops packages.
func ArchSlug() string {
	return "amd64"
}