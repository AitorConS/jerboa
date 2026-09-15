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
func loadOpsPackageEnvs(pkgRefs []string, archs ...string) map[string]string {
	opsStore, err := openOpsStore(optionalPackageArch(archs))
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
		target := manifest.LookupArch(id.Namespace, id.Name, id.Version, optionalPackageArch(archs))
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

// mergePkgRefs concatenates package refs from unikernel.toml and --pkg flags,
// dropping exact duplicates while preserving order (toml refs first).
func mergePkgRefs(fromConfig, fromFlags []string) []string {
	seen := make(map[string]bool, len(fromConfig)+len(fromFlags))
	var out []string
	for _, ref := range append(append([]string{}, fromConfig...), fromFlags...) {
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

// loadOpsProgramDefaults reads Program/Args from each ops package's
// package.manifest and returns the first declared program with its arguments.
// In ops manifests Args[0] is the program's own in-image path (argv[0]) and the
// remaining elements are the real arguments — e.g. eyberg/postgresql ships
// Args: ["/usr/local/pgsql/bin/postgres", "-D", "db"]. This lets a raw build
// run an ops package with no [program] section in unikernel.toml: the package
// already declares how it is meant to be started.
// Errors are silently skipped — a missing or malformed manifest simply yields
// no default, and the caller reports the actionable error.
func loadOpsProgramDefaults(pkgRefs []string, archs ...string) (path string, args []string) {
	opsStore, err := openOpsStore(optionalPackageArch(archs))
	if err != nil {
		return "", nil
	}
	manifest, err := opsStore.FetchManifestCached()
	if err != nil {
		return "", nil
	}
	for _, ref := range pkgRefs {
		id, parseErr := pkg.ParseOpsIdentifier(ref)
		if parseErr != nil {
			continue
		}
		target := manifest.LookupArch(id.Namespace, id.Name, id.Version, optionalPackageArch(archs))
		if target == nil {
			continue
		}
		cfg, loadErr := opsStore.LoadPackageManifest(target.Namespace, target.Name, target.Version)
		if loadErr != nil {
			continue
		}
		switch {
		case len(cfg.Args) > 0:
			return cfg.Args[0], cfg.Args[1:]
		case cfg.Program != "":
			// Program may be "pkg_version/binary" (relative to the ops packages
			// root); the basename resolves through findProgramBinary.
			return filepath.Base(cfg.Program), nil
		}
	}
	return "", nil
}

// resolvePackages downloads and extracts packages, returning the list of
// package files that should be included in the manifest.
func resolvePackages(ctx context.Context, pkgRefs []string) ([]pkg.File, error) { //nolint:unparam // ctx reserved for future cancellation
	store, err := pkg.NewStore(pkgStorePath())
	if err != nil {
		return nil, err
	}
	var files []pkg.File
	for _, ref := range pkgRefs {
		name, version := parsePkgRef(ref)
		resolved, identity, err := store.Resolve(name, version, "linux/"+packageArchFor(ctx))
		if err != nil {
			return nil, err
		}
		for i := range resolved {
			r := identity
			resolved[i].Reference = &r
		}
		files = append(files, resolved...)
	}
	return files, nil
}

// resolveOpsPackages downloads and extracts ops packages, returning the list
// of package files with proper guest paths (preserving sysroot/ hierarchy).
func resolveOpsPackages(ctx context.Context, pkgRefs []string) ([]pkg.File, error) { //nolint:unparam // ctx reserved for future cancellation
	opsStore, err := openOpsStore(packageArchFor(ctx))
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

		target := manifest.LookupArch(id.Namespace, id.Name, id.Version, packageArchFor(ctx))
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
		if err := pkg.ValidateFiles(pkgFiles, "linux/"+packageArchFor(ctx)); err != nil {
			return nil, err
		}
		archiveHash, err := opsStore.ArchiveSHA256(target.Namespace, target.Name, target.Version, target.SHA256)
		if err != nil {
			return nil, err
		}
		for i := range pkgFiles {
			pkgFiles[i].Reference = &pkg.Reference{Source: "ops", Name: target.Namespace + "/" + target.Name, Version: target.Version, Platform: "linux/" + packageArchFor(ctx), SHA256: archiveHash}
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
			if sameRuntimeFamily(userName, autoName) {
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

// sameRuntimeFamily reports whether two runtime base names refer to the same
// runtime, allowing only a trailing numeric version to differ (python/python3,
// node/node20). It does not treat arbitrary shared prefixes as equivalent, so
// "node-exporter" does not cover "node".
func sameRuntimeFamily(a, b string) bool {
	if a == b {
		return true
	}
	long, short := a, b
	if len(b) > len(a) {
		long, short = b, a
	}
	if !strings.HasPrefix(long, short) {
		return false
	}
	rest := long[len(short):]
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return rest != ""
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

	return resolvePackages(ctx, autoPkgs)
}

func lookupOpsPackage(manifest *pkg.OpsPackageList, name, version string, archs ...string) *pkg.OpsPackage {
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
			if t := manifest.LookupArch(ns, alias, version, optionalPackageArch(archs)); t != nil {
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
				if p.Namespace != ns || p.Name != alias || (p.Arch != "" && p.Arch != optionalPackageArch(archs) && (p.Arch != "x86_64" || optionalPackageArch(archs) != "amd64")) || (p.Arch == "" && optionalPackageArch(archs) != "amd64") {
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
	opsStore, err := openOpsStore(packageArchFor(ctx))
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
			target = manifest.LookupArch(id.Namespace, id.Name, id.Version, packageArchFor(ctx))
		} else {
			target = lookupOpsPackage(manifest, pkgName, pkgVer, packageArchFor(ctx))
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
		if err := pkg.ValidateFiles(pkgFiles, "linux/"+packageArchFor(ctx)); err != nil {
			return nil, err
		}
		archiveHash, err := opsStore.ArchiveSHA256(target.Namespace, target.Name, target.Version, target.SHA256)
		if err != nil {
			return nil, err
		}
		for i := range pkgFiles {
			pkgFiles[i].Reference = &pkg.Reference{Source: "ops", Name: target.Namespace + "/" + target.Name, Version: target.Version, Platform: "linux/" + packageArchFor(ctx), SHA256: archiveHash}
		}
		files = append(files, pkgFiles...)
	}
	return files, nil
}

// The target travels with a build context, without mutating process environment.
type packagePlatformKey struct{}

func packageArchFor(ctx context.Context) string {
	if arch, ok := ctx.Value(packagePlatformKey{}).(string); ok {
		return arch
	}
	return pkg.ArchSlug()
}
func optionalPackageArch(archs []string) string {
	if len(archs) > 0 {
		return archs[0]
	}
	return pkg.ArchSlug()
}
func packageReferences(files []pkg.File) []pkg.Reference {
	refs := []pkg.Reference{}
	seen := map[string]bool{}
	for _, f := range files {
		if f.Reference != nil {
			r := *f.Reference
			key := r.Source + "/" + r.Name + ":" + r.Version + "/" + r.Platform
			if !seen[key] {
				seen[key] = true
				refs = append(refs, r)
			}
		}
	}
	return refs
}
