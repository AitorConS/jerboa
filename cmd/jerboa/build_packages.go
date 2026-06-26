package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	pkg "github.com/AitorConS/jerboa/internal/package"
)

// loadOpsPackageEnvs reads the Env field from each ops package's package.manifest
// and returns a merged map. Errors are silently skipped — missing or malformed
// manifests must not block the build.
func loadOpsPackageEnvs(pkgRefs []string) map[string]string {
	opsStore, err := openOpsStore()
	if err != nil {
		return nil
	}

	manifest, err := opsStore.FetchManifestCached()
	if err != nil {
		return nil
	}

	env := make(map[string]string)
	for _, ref := range pkgRefs {
		id, parseErr := pkg.ParseOpsIdentifier(ref)
		if parseErr != nil {
			continue
		}
		target := manifest.Lookup(id.Namespace, id.Name, id.Version)
		if target == nil {
			continue
		}
		cfg, loadErr := opsStore.LoadPackageManifest(target.Namespace, target.Name, target.Version)
		if loadErr != nil {
			continue
		}
		for k, v := range cfg.Env {
			env[k] = v
		}
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

// resolvePackages downloads and extracts packages, returning the list of
// package files that should be included in the manifest.
func resolvePackages(ctx context.Context, pkgRefs []string) ([]pkg.File, error) { //nolint:unparam // ctx reserved for future cancellation
	pkgStore, err := pkg.NewStore(pkgStorePath())
	if err != nil {
		return nil, fmt.Errorf("open package store: %w", err)
	}

	idx, err := pkg.FetchIndex()
	if err != nil {
		return nil, fmt.Errorf("fetch package index: %w", err)
	}

	var files []pkg.File
	for _, ref := range pkgRefs {
		pkgName, pkgVer := parsePkgRef(ref)
		target := idx.Latest(pkgName)
		if target == nil {
			return nil, fmt.Errorf("package %q not found in index", pkgName)
		}
		if pkgVer != "" {
			found := false
			versions, ok := idx.Packages[pkgName]
			if ok {
				for i := range versions {
					if versions[i].Version == pkgVer {
						target = &versions[i]
						found = true
						break
					}
				}
			}
			if !found {
				return nil, fmt.Errorf("version %q of package %q not found", pkgVer, pkgName)
			}
		}
		if !pkgStore.IsDownloaded(target.Name, target.Version) {
			if err := pkgStore.Download(*target); err != nil {
				return nil, fmt.Errorf("download package %s: %w", target.Name, err)
			}
			if err := pkgStore.SaveMeta(*target); err != nil {
				return nil, fmt.Errorf("save package meta: %w", err)
			}
		}
		if !pkgStore.IsExtracted(target.Name, target.Version) {
			if err := pkgStore.Extract(*target); err != nil {
				return nil, fmt.Errorf("extract package %s: %w", target.Name, err)
			}
		}
		paths, err := pkgStore.ExtractedFiles(target.Name, target.Version)
		if err != nil {
			return nil, fmt.Errorf("list package files %s: %w", target.Name, err)
		}
		for _, p := range paths {
			files = append(files, pkg.File{HostPath: p, GuestPath: filepath.Base(p)})
		}
	}
	return files, nil
}

// resolveOpsPackages downloads and extracts ops packages, returning the list
// of package files with proper guest paths (preserving sysroot/ hierarchy).
func resolveOpsPackages(ctx context.Context, pkgRefs []string) ([]pkg.File, error) { //nolint:unparam // ctx reserved for future cancellation
	opsStore, err := openOpsStore()
	if err != nil {
		return nil, fmt.Errorf("open ops package store: %w", err)
	}

	manifest, err := opsStore.FetchManifestCached()
	if err != nil {
		return nil, fmt.Errorf("fetch ops manifest: %w", err)
	}

	var files []pkg.File
	for _, ref := range pkgRefs {
		id, err := pkg.ParseOpsIdentifier(ref)
		if err != nil {
			return nil, fmt.Errorf("parse ops package %q: %w", ref, err)
		}

		target := manifest.Lookup(id.Namespace, id.Name, id.Version)
		if target == nil {
			return nil, fmt.Errorf("ops package %q not found in manifest", ref)
		}

		if !opsStore.IsDownloaded(target.Namespace, target.Name, target.Version) {
			if err := opsStore.Download(target.Namespace, target.Name, target.Version, target.SHA256); err != nil {
				return nil, fmt.Errorf("download ops package %s: %w", target.Name, err)
			}
		}
		if !opsStore.IsExtracted(target.Namespace, target.Name, target.Version) {
			if err := opsStore.Extract(target.Namespace, target.Name, target.Version); err != nil {
				return nil, fmt.Errorf("extract ops package %s: %w", target.Name, err)
			}
		}

		pkgFiles, err := opsStore.ExtractedFiles(target.Namespace, target.Name, target.Version)
		if err != nil {
			return nil, fmt.Errorf("list ops package files %s: %w", target.Name, err)
		}
		files = append(files, pkgFiles...)
	}
	return files, nil
}

// filterCoveredAutoPkgs removes auto-packages whose base name is already
// covered by a user-provided package, so the driver doesn't double-resolve.
// Uses prefix matching so "python3" (user) covers "python" (auto) and vice versa.
func filterCoveredAutoPkgs(autoPkgs, userPkgs []string) []string {
	if len(userPkgs) == 0 {
		return autoPkgs
	}
	userBaseNames := make([]string, 0, len(userPkgs))
	for _, ref := range userPkgs {
		name, _ := parsePkgRef(ref)
		if idx := strings.LastIndex(name, "/"); idx >= 0 {
			name = name[idx+1:]
		}
		userBaseNames = append(userBaseNames, name)
	}
	filtered := autoPkgs[:0:0]
	for _, ref := range autoPkgs {
		autoName, _ := parsePkgRef(ref)
		if idx := strings.LastIndex(autoName, "/"); idx >= 0 {
			autoName = autoName[idx+1:]
		}
		covered := false
		for _, userName := range userBaseNames {
			// Exact match or prefix relationship catches python/python3/python2 variants.
			if userName == autoName || strings.HasPrefix(userName, autoName) || strings.HasPrefix(autoName, userName) {
				covered = true
				break
			}
		}
		if !covered {
			filtered = append(filtered, ref)
		}
	}
	return filtered
}

// resolveAutoPackages resolves language runtime packages (e.g. "node:20")
// and returns the list of extracted package files.
func resolveAutoPackages(ctx context.Context, autoPkgs []string, pkgSource string) ([]pkg.File, error) {
	if len(autoPkgs) == 0 {
		return nil, nil
	}

	if pkgSource == "ops" {
		return resolveOpsAutoPackages(ctx, autoPkgs)
	}

	pkgStore, err := pkg.NewStore(pkgStorePath())
	if err != nil {
		return nil, fmt.Errorf("open package store: %w", err)
	}

	idx, err := pkg.FetchIndex()
	if err != nil {
		return nil, fmt.Errorf("fetch package index: %w", err)
	}

	var files []pkg.File
	for _, ref := range autoPkgs {
		pkgName, pkgVer := parsePkgRef(ref)
		target := idx.Latest(pkgName)
		if target == nil {
			return nil, fmt.Errorf("package %q not found in index", pkgName)
		}
		if pkgVer != "" {
			found := false
			versions, ok := idx.Packages[pkgName]
			if ok {
				for i := range versions {
					if versions[i].Version == pkgVer {
						target = &versions[i]
						found = true
						break
					}
				}
			}
			if !found {
				return nil, fmt.Errorf("version %q of package %q not found", pkgVer, pkgName)
			}
		}
		if !pkgStore.IsDownloaded(target.Name, target.Version) {
			if err := pkgStore.Download(*target); err != nil {
				return nil, fmt.Errorf("download package %s: %w", target.Name, err)
			}
			if err := pkgStore.SaveMeta(*target); err != nil {
				return nil, fmt.Errorf("save package meta: %w", err)
			}
		}
		if !pkgStore.IsExtracted(target.Name, target.Version) {
			if err := pkgStore.Extract(*target); err != nil {
				return nil, fmt.Errorf("extract package %s: %w", target.Name, err)
			}
		}
		paths, err := pkgStore.ExtractedFiles(target.Name, target.Version)
		if err != nil {
			return nil, fmt.Errorf("list package files %s: %w", target.Name, err)
		}
		for _, p := range paths {
			files = append(files, pkg.File{HostPath: p, GuestPath: filepath.Base(p)})
		}
	}
	return files, nil
}

func lookupOpsPackage(manifest *pkg.OpsPackageList, name, version string) *pkg.OpsPackage {
	// Build a list of name aliases to try: the ops ecosystem names the Python
	// runtime "python3" rather than "python", so try both.
	names := []string{name}
	switch name {
	case "python":
		names = append(names, "python3", "python2")
	case "python3":
		names = append(names, "python")
	case "python2":
		names = append(names, "python")
	}

	namespaces := []string{"eyberg", "nanovms", "myuniverse"}
	for _, alias := range names {
		for _, ns := range namespaces {
			if t := manifest.Lookup(ns, alias, version); t != nil {
				return t
			}
		}
	}
	if version == "" || version == "latest" {
		return nil
	}
	for _, alias := range names {
		for _, ns := range namespaces {
			for i := range manifest.Packages {
				p := &manifest.Packages[i]
				if p.Namespace != ns || p.Name != alias {
					continue
				}
				pv := strings.TrimPrefix(p.Version, "v")
				if strings.HasPrefix(pv, version+".") || strings.HasPrefix(pv, version+"-") || pv == version {
					return p
				}
			}
		}
	}
	return nil
}

func resolveOpsAutoPackages(ctx context.Context, autoPkgs []string) ([]pkg.File, error) { //nolint:unparam // ctx reserved for future cancellation
	opsStore, err := openOpsStore()
	if err != nil {
		return nil, fmt.Errorf("open ops package store: %w", err)
	}

	manifest, err := opsStore.FetchManifestCached()
	if err != nil {
		return nil, fmt.Errorf("fetch ops manifest: %w", err)
	}

	var files []pkg.File
	for _, ref := range autoPkgs {
		pkgName, pkgVer := parsePkgRef(ref)

		var target *pkg.OpsPackage
		if strings.Contains(pkgName, "/") {
			id, parseErr := pkg.ParseOpsIdentifier(pkgName)
			if parseErr != nil {
				return nil, fmt.Errorf("parse ops package %q: %w", pkgName, parseErr)
			}
			if pkgVer != "" && pkgVer != "latest" {
				id.Version = pkgVer
			}
			target = manifest.Lookup(id.Namespace, id.Name, id.Version)
		} else {
			target = lookupOpsPackage(manifest, pkgName, pkgVer)
		}
		if target == nil {
			return nil, fmt.Errorf("ops package %q not found in manifest (try --pkg eyberg/%s)", ref, pkgName)
		}

		if !opsStore.IsDownloaded(target.Namespace, target.Name, target.Version) {
			if err := opsStore.Download(target.Namespace, target.Name, target.Version, target.SHA256); err != nil {
				return nil, fmt.Errorf("download ops package %s: %w", target.Name, err)
			}
		}
		if !opsStore.IsExtracted(target.Namespace, target.Name, target.Version) {
			if err := opsStore.Extract(target.Namespace, target.Name, target.Version); err != nil {
				return nil, fmt.Errorf("extract ops package %s: %w", target.Name, err)
			}
		}

		pkgFiles, err := opsStore.ExtractedFiles(target.Namespace, target.Name, target.Version)
		if err != nil {
			return nil, fmt.Errorf("list ops package files %s: %w", target.Name, err)
		}
		files = append(files, pkgFiles...)
	}
	return files, nil
}
