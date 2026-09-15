package pkg

import (
	"archive/tar"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// LocalFiles resolves a program without executing it or a Linux loader.
func LocalFiles(binary, sysroot, program string, extras []File) ([]File, string, error) {
	if program == "" {
		program = filepath.Base(binary)
	}
	var err error
	if sysroot != "" {
		sysroot, err = filepath.Abs(sysroot)
		if err != nil {
			return nil, "", fmt.Errorf("local operation: %w", err)
		}
	}
	resolveInput := func(input string) (string, error) {
		absolute, err := filepath.Abs(input)
		if err != nil {
			return "", fmt.Errorf("local operation: %w", err)
		}
		if sysroot == "" || !isUnderDir(sysroot, absolute) {
			return absolute, nil
		}
		relative, err := filepath.Rel(sysroot, absolute)
		if err != nil {
			return "", fmt.Errorf("local operation: %w", err)
		}
		source := &containerFS{root: sysroot, index: map[string]cfsEntry{}}
		real, err := source.resolve(relative)
		if err != nil {
			return "", err
		}
		return filepath.Join(sysroot, real), nil
	}
	binary, err = resolveInput(binary)
	if err != nil {
		return nil, "", err
	}
	extras = append([]File{}, extras...)
	for i := range extras {
		if !extras[i].IsDir {
			extras[i].HostPath, err = resolveInput(extras[i].HostPath)
			if err != nil {
				return nil, "", err
			}
		}
	}
	platform, err := ELFPlatform(binary)
	if err != nil {
		return nil, "", err
	}
	c := &containerFS{root: sysroot, index: map[string]cfsEntry{}, hosts: map[string]string{}}
	if sysroot == "" && runtime.GOOS == "linux" {
		c.root = "/"
	}
	gp := absClean(program)
	c.index[gp] = cfsEntry{typeflag: tar.TypeReg}
	c.hosts[gp] = binary
	for _, f := range extras {
		dest := absClean(f.GuestPath)
		if _, ok := c.index[dest]; !ok {
			c.index[dest] = cfsEntry{typeflag: tar.TypeReg}
			c.hosts[dest] = f.HostPath
		}
	}
	closure, err := c.elfClosure(gp)
	if err != nil {
		return nil, "", err
	}
	files := []File{}
	for _, p := range closure {
		real, err := c.resolve(p)
		if err != nil {
			return nil, "", err
		}
		host := c.hosts[real]
		if host == "" {
			host = filepath.Join(c.root, real)
		}
		files = append(files, File{HostPath: host, GuestPath: strings.TrimPrefix(p, "/")})
	}
	files = append(files, extras...)
	if err := ValidateFiles(files, platform); err != nil {
		return nil, "", err
	}
	return files, platform, nil
}

// Resolve checks local content first; a local hit performs no network request.
func (s *Store) Resolve(name, version, platform string) ([]File, Reference, error) {
	if err := ValidatePlatform(platform); err != nil {
		return nil, Reference{}, err
	}
	local, err := s.List()
	if err != nil {
		return nil, Reference{}, err
	}
	sort.SliceStable(local, func(i, j int) bool { return local[i].Created.After(local[j].Created) })
	matches := func(p Package) bool {
		return p.Name == name && (version == "" || version == "latest" || p.Version == version) && (p.Platform == "" || p.Platform == platform)
	}
	var legacyErr error
	for _, p := range local {
		if matches(p) {
			files, ref, err := s.Verify(p, platform)
			if err == nil {
				return files, ref, nil
			}
			if !errors.Is(err, ErrPlatformMismatch) {
				return nil, Reference{}, err
			}
			legacyErr = err
		}
	}
	idx, err := s.FetchIndexCached()
	if err != nil {
		if legacyErr != nil {
			return nil, Reference{}, legacyErr
		}
		return nil, Reference{}, err
	}
	for _, p := range idx.Packages[name] {
		if matches(p) {
			selected := s
			if p.Platform != "" {
				selected, err = s.ForPlatform(p.Platform)
				if err != nil {
					return nil, Reference{}, err
				}
			}
			if !selected.IsDownloaded(p.Name, p.Version) {
				if err := selected.Download(p); err != nil {
					return nil, Reference{}, err
				}
				if err := selected.SaveMeta(p); err != nil {
					return nil, Reference{}, err
				}
			}
			files, ref, err := s.Verify(p, platform)
			if errors.Is(err, ErrPlatformMismatch) {
				legacyErr = err
				continue
			}
			return files, ref, err
		}
	}
	if legacyErr != nil {
		return nil, Reference{}, legacyErr
	}
	return nil, Reference{}, fmt.Errorf("package %s:%s for %s not found", name, version, platform)
}
