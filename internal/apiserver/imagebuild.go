//go:build linux || (darwin && arm64)

package apiserver

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/image"
	pkg "github.com/AitorConS/jerboa/internal/package"
	"github.com/AitorConS/jerboa/internal/volume"
)

// SetImageStore attaches the daemon's image store, enabling image resolution
// for VM.Run as well as the Image.List and Image.Remove RPCs. Call once after
// NewServer, before Serve.
func (s *Server) SetImageStore(store *image.Store) {
	s.imgStore = store
}

// EnableImageBuild turns on the Image.Build RPC with a fixed mkfs function. It
// requires an image store (see SetImageStore). Mostly used in tests; the daemon
// uses EnableImageBuildResolver to resolve mkfs lazily.
func (s *Server) EnableImageBuild(mkfs image.MkfsFunc) {
	s.EnableImageBuildResolver(func(context.Context) (image.MkfsFunc, error) { return mkfs, nil })
}

// EnableImageBuildResolver turns on the Image.Build RPC with a resolver that
// produces the mkfs function on first use. This lets the daemon download the
// kernel toolchain on the first build instead of blocking startup. A successful
// result is cached; errors are not, so a failed download can be retried.
func (s *Server) EnableImageBuildResolver(resolver func(context.Context) (image.MkfsFunc, error)) {
	s.mkfsMu.Lock()
	s.mkfsResolver = resolver
	s.mkfsCached = nil
	s.mkfsMu.Unlock()
}

// imageBuildEnabled reports whether an mkfs resolver is configured.
func (s *Server) imageBuildEnabled() bool {
	s.mkfsMu.Lock()
	defer s.mkfsMu.Unlock()
	return s.mkfsResolver != nil
}

// resolveMkfs returns the mkfs function, invoking the resolver once and caching
// the result. Errors are returned without caching so a retry can succeed.
func (s *Server) resolveMkfs(ctx context.Context) (image.MkfsFunc, error) {
	s.mkfsMu.Lock()
	defer s.mkfsMu.Unlock()
	if s.mkfsResolver == nil {
		return nil, nil
	}
	if s.mkfsCached != nil {
		return s.mkfsCached, nil
	}
	f, err := s.mkfsResolver(ctx)
	if err != nil {
		return nil, err
	}
	s.mkfsCached = f
	return f, nil
}

// EnableVolumeFormatResolver configures a resolver that produces the volume
// formatter (mkfs-based) on first use, so the daemon can format attached
// volumes as TFS at run time. A successful result is cached; errors are not.
func (s *Server) EnableVolumeFormatResolver(resolver func(context.Context) (volume.Formatter, error)) {
	s.volFmtMu.Lock()
	s.volFmtResolv = resolver
	s.volFmtCached = nil
	s.volFmtMu.Unlock()
}

// resolveVolumeFormatter returns the volume formatter, invoking the resolver
// once and caching the result. Returns nil (no error) when no resolver is set.
func (s *Server) resolveVolumeFormatter(ctx context.Context) (volume.Formatter, error) {
	s.volFmtMu.Lock()
	defer s.volFmtMu.Unlock()
	if s.volFmtResolv == nil {
		return nil, nil
	}
	if s.volFmtCached != nil {
		return s.volFmtCached, nil
	}
	f, err := s.volFmtResolv(ctx)
	if err != nil {
		return nil, err
	}
	s.volFmtCached = f
	return f, nil
}

// EnableVolumeSeedResolver configures a resolver that produces the volume seeder
// (mkfs-based) on first use, so the daemon can populate a volume's disk with an
// initialized filesystem. A successful result is cached; errors are not.
func (s *Server) EnableVolumeSeedResolver(resolver func(context.Context) (volume.Seeder, error)) {
	s.volSeedMu.Lock()
	s.volSeedResol = resolver
	s.volSeedCache = nil
	s.volSeedMu.Unlock()
}

// resolveVolumeSeeder returns the volume seeder, invoking the resolver once and
// caching the result. Returns nil (no error) when no resolver is set.
func (s *Server) resolveVolumeSeeder(ctx context.Context) (volume.Seeder, error) {
	s.volSeedMu.Lock()
	defer s.volSeedMu.Unlock()
	if s.volSeedResol == nil {
		return nil, nil
	}
	if s.volSeedCache != nil {
		return s.volSeedCache, nil
	}
	f, err := s.volSeedResol(ctx)
	if err != nil {
		return nil, err
	}
	s.volSeedCache = f
	return f, nil
}

// handleVolumeSeed reads a streamed build context and populates the requested
// volume's disk with a TFS filesystem built from those files, via mkfs. The
// response (or error) is written directly to conn; the connection is consumed
// and not reused afterwards (mirrors handleBuild).
func (s *Server) handleVolumeSeed(ctx context.Context, params json.RawMessage, stream io.Reader, conn net.Conn, reqID int64) {
	var p api.VolumeSeedParams
	if err := json.Unmarshal(params, &p); err != nil {
		drain(stream)
		s.writeError(conn, reqID, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()})
		return
	}
	if p.VolumeName == "" || p.DiskPath == "" || p.Label == "" {
		drain(stream)
		s.writeError(conn, reqID, &api.RPCError{Code: -32602, Message: "volume_name, disk_path, and label are required"})
		return
	}
	if s.volStore == nil {
		drain(stream)
		s.writeError(conn, reqID, &api.RPCError{Code: -32601, Message: "method not found: Volume.Seed (volume store disabled)"})
		return
	}

	// The disk to seed is the client-supplied, daemon-visible path — the same path
	// `jerboa run -v` attaches. On Windows the volume lives on the host filesystem
	// (reachable by the daemon under /mnt/<drive>/…) and is absent from the
	// daemon's own volume store, so re-resolving it there fails and made seeding
	// impossible on the only supported Windows path. When the
	// daemon DOES own the volume (Linux-native, shared store) the paths are
	// cross-checked so a client cannot redirect the seed onto another volume's disk.
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	diskPath := p.DiskPath
	if vol, err := s.volStore.Get(p.VolumeName); err == nil {
		if filepath.Clean(p.DiskPath) != filepath.Clean(vol.DiskPath) {
			drain(stream)
			s.writeError(conn, reqID, &api.RPCError{Code: -32602, Message: "disk_path does not match the requested volume"})
			return
		}
		diskPath = vol.DiskPath
	}

	// Seeding writes a filesystem into an existing disk image; it never creates the
	// backing file. Require it to exist so a typo'd name or an unseeded create fails
	// with a clear message instead of erroring deep inside mkfs.
	if info, statErr := os.Stat(diskPath); statErr != nil || info.IsDir() {
		drain(stream)
		s.writeError(conn, reqID, &api.RPCError{Code: -32602, Message: fmt.Sprintf(
			"volume disk %q not found; create the volume first with 'jerboa volume create %s'", diskPath, p.VolumeName)})
		return
	}

	if err := s.volumeUnused(diskPath); err != nil {
		drain(stream)
		s.writeError(conn, reqID, &api.RPCError{Code: -32000, Message: err.Error()})
		return
	}
	seeder, err := s.resolveVolumeSeeder(ctx)
	if err != nil {
		drain(stream)
		s.writeError(conn, reqID, &api.RPCError{Code: -32000, Message: "resolve volume seeder: " + err.Error()})
		return
	}
	if seeder == nil {
		drain(stream)
		s.writeError(conn, reqID, &api.RPCError{Code: -32601, Message: "method not found: Volume.Seed (seeding disabled)"})
		return
	}

	tmpDir, err := os.MkdirTemp("", "jerboa-seed-ctx-*")
	if err != nil {
		drain(stream)
		s.writeError(conn, reqID, &api.RPCError{Code: -32000, Message: "create seed context dir: " + err.Error()})
		return
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	files, err := extractBuildContext(stream, tmpDir)
	if err != nil {
		drain(stream)
		s.writeError(conn, reqID, &api.RPCError{Code: -32000, Message: "extract seed context: " + err.Error()})
		return
	}
	if len(files) == 0 {
		drain(stream)
		s.writeError(conn, reqID, &api.RPCError{Code: -32602, Message: "seed context is empty"})
		return
	}

	manifest := image.BuildVolumeManifest(files)
	if err := seeder(ctx, diskPath, p.Label, p.SizeBytes, manifest); err != nil {
		drain(stream)
		s.writeError(conn, reqID, &api.RPCError{Code: -32000, Message: err.Error()})
		return
	}

	// extractBuildContext stops at the tar's EOF; drain the client's frame
	// terminator before responding so its write does not race the connection
	// close (broken pipe) — the same reason handleBuild drains.
	drain(stream)

	var size int64
	if st, statErr := os.Stat(diskPath); statErr == nil {
		size = st.Size()
	}
	resp := api.Response{JSONRPC: "2.0", ID: reqID}
	raw, mErr := json.Marshal(api.VolumeSeedResult{DiskPath: diskPath, SizeBytes: size})
	if mErr != nil {
		s.writeError(conn, reqID, &api.RPCError{Code: -32000, Message: "marshal result: " + mErr.Error()})
		return
	}
	resp.Result = raw
	_ = json.NewEncoder(conn).Encode(resp)
}

// handleImageList returns the manifests held in the daemon's image store.
func (s *Server) handleImageList() (any, *api.RPCError) {
	if s.imgStore == nil {
		return nil, &api.RPCError{Code: -32601, Message: "method not found: Image.List (image store disabled)"}
	}
	manifests, err := s.imgStore.List()
	if err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	out := make([]api.ImageManifestResult, len(manifests))
	for i, m := range manifests {
		out[i] = imageManifestResult(m)
	}
	return out, nil
}

// handleImageGet returns the manifest for a single reference.
func (s *Server) handleImageGet(params json.RawMessage) (any, *api.RPCError) {
	if s.imgStore == nil {
		return nil, &api.RPCError{Code: -32601, Message: "method not found: Image.Get (image store disabled)"}
	}
	var p struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	m, _, err := s.imgStore.Get(p.Ref)
	if err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	return imageManifestResult(m), nil
}

// handleImageRemove deletes a name:tag (or sha) reference from the store.
func (s *Server) handleImageRemove(params json.RawMessage) (any, *api.RPCError) {
	if s.imgStore == nil {
		return nil, &api.RPCError{Code: -32601, Message: "method not found: Image.Remove (image store disabled)"}
	}
	var p struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	// Refuse to remove an image that a VM still references, like `docker rmi`.
	// A VM created from this image would otherwise be left pointing at a disk
	// that no longer exists. Match by resolved disk path so any ref (name,
	// name:tag, or digest) pointing at the same image is caught.
	if s.mgr != nil {
		if _, targetDisk, err := s.imgStore.Get(p.Ref); err == nil {
			for _, v := range s.mgr.List() {
				ref := v.Cfg.ImageRef
				if ref == "" {
					continue
				}
				if _, vmDisk, err := s.imgStore.Get(ref); err == nil && vmDisk == targetDisk {
					name := v.Cfg.Name
					if name == "" {
						name = v.ID
					}
					return nil, &api.RPCError{Code: -32000, Message: fmt.Sprintf(
						"image %q is in use by VM %s (%s); remove the VM first", p.Ref, name, v.GetState())}
				}
			}
		}
	}
	if err := s.imgStore.Remove(p.Ref); err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	return map[string]string{"status": "ok"}, nil
}

// imageManifestResult converts a stored manifest to its wire representation.
func imageManifestResult(m image.Manifest) api.ImageManifestResult {
	return api.ImageManifestResult{
		Name:       m.Name,
		Tag:        m.Tag,
		DiskDigest: m.DiskDigest,
		DiskSize:   m.DiskSize,
		Created:    m.Created.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

// handleBuild reads a streamed build context, assembles a disk image with mkfs
// on the local filesystem, and stores it. The response (or error) is written
// directly to conn; the connection is consumed and not reused afterwards.
func (s *Server) handleBuild(ctx context.Context, params json.RawMessage, stream io.Reader, conn net.Conn, reqID int64) {
	var p api.BuildParams
	if err := json.Unmarshal(params, &p); err != nil {
		drain(stream)
		s.writeError(conn, reqID, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()})
		return
	}
	if s.imgStore == nil || !s.imageBuildEnabled() {
		drain(stream)
		s.writeError(conn, reqID, &api.RPCError{Code: -32601, Message: "method not found: Image.Build (image build disabled)"})
		return
	}

	tmpDir, err := os.MkdirTemp("", "jerboa-build-ctx-*")
	if err != nil {
		drain(stream)
		s.writeError(conn, reqID, &api.RPCError{Code: -32000, Message: "create build context dir: " + err.Error()})
		return
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	files, err := extractBuildContext(stream, tmpDir)
	if err != nil {
		s.writeError(conn, reqID, &api.RPCError{Code: -32000, Message: "extract build context: " + err.Error()})
		return
	}

	binaryPath, pkgFiles, err := splitProgram(files, p.Program)
	if err != nil {
		// extractBuildContext stops at the tar's EOF, leaving the frame
		// terminator unread; drain it so the client's stream close does not race
		// the connection closing (broken pipe) before it reads this error.
		drain(stream)
		s.writeError(conn, reqID, &api.RPCError{Code: -32602, Message: err.Error()})
		return
	}

	// Resolve mkfs lazily — the first build may download the kernel toolchain.
	mkfs, err := s.resolveMkfs(ctx)
	if err != nil {
		drain(stream)
		s.writeError(conn, reqID, &api.RPCError{Code: -32000, Message: "resolve mkfs toolchain: " + err.Error()})
		return
	}

	m, err := image.NewBuilder(s.imgStore).Build(ctx, image.BuildConfig{
		Name:        p.Name,
		Tag:         p.Tag,
		BinaryPath:  binaryPath,
		ProgramPath: p.ProgramPath,
		MkfsRun:     mkfs,
		Memory:      p.Memory,
		CPUs:        p.CPUs,
		PkgFiles:    pkgFiles,
		Entrypoint:  p.Entrypoint,
		Args:        p.Args,
		Env:         p.Env,
		Port:        p.Port,
		Ports:       p.Ports,
		DiskSize:    p.DiskSize,
		Output:      io.Discard,
	})
	if err != nil {
		drain(stream)
		s.writeError(conn, reqID, &api.RPCError{Code: -32000, Message: err.Error()})
		return
	}

	// extractBuildContext stopped at the tar's EOF, but the client's frame
	// terminator (written by FrameWriter.Close after the last tar byte) is still
	// in flight. Drain it before responding so the client's terminator write does
	// not race the connection close (broken pipe) — the same reason the error
	// paths above drain.
	drain(stream)

	resp := api.Response{JSONRPC: "2.0", ID: reqID}
	raw, mErr := json.Marshal(imageManifestResult(m))
	if mErr != nil {
		s.writeError(conn, reqID, &api.RPCError{Code: -32000, Message: "marshal result: " + mErr.Error()})
		return
	}
	resp.Result = raw
	_ = json.NewEncoder(conn).Encode(resp)
}

// extractBuildContext untars stream into dir and returns the extracted files as
// pkg.File entries (HostPath = on-disk location, GuestPath = tar path). It
// rejects entries that escape dir.
// maxBuildContextBytes caps the total size of an extracted build/seed context
// written to the daemon's temp directory. The per-frame limit in the framing
// layer bounds a single chunk, but not the cumulative stream, so without this an
// authenticated or local client could exhaust the daemon's disk. It is a var so
// tests can lower it and a future flag can raise it.
var maxBuildContextBytes int64 = 4 << 30 // 4 GiB

func extractBuildContext(stream io.Reader, dir string) ([]pkg.File, error) {
	tr := tar.NewReader(stream)
	var files []pkg.File
	remaining := maxBuildContextBytes
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		guestPath := filepath.ToSlash(filepath.Clean("/" + strings.TrimSuffix(hdr.Name, "/")))
		switch hdr.Typeflag {
		case tar.TypeDir:
			dest, err := safeJoin(dir, guestPath)
			if err != nil {
				return nil, err
			}
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return nil, fmt.Errorf("mkdir %s: %w", hdr.Name, err)
			}
			files = append(files, pkg.File{GuestPath: strings.TrimPrefix(guestPath, "/"), IsDir: true})
		case tar.TypeReg:
			dest, err := safeJoin(dir, guestPath)
			if err != nil {
				return nil, err
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return nil, fmt.Errorf("mkdir for %s: %w", hdr.Name, err)
			}
			mode := os.FileMode(hdr.Mode).Perm() //nolint:gosec // tar mode bits are bounded by Perm()
			n, err := writeFileFromTar(dest, tr, mode, remaining)
			if err != nil {
				return nil, err
			}
			remaining -= n
			files = append(files, pkg.File{HostPath: dest, GuestPath: strings.TrimPrefix(guestPath, "/")})
		default:
			continue
		}
	}
	return files, nil
}

// writeFileFromTar copies the current tar entry to dest, writing at most budget
// bytes. It returns the number of bytes written and an error if the cumulative
// build context would exceed maxBuildContextBytes.
func writeFileFromTar(dest string, tr io.Reader, mode os.FileMode, budget int64) (int64, error) {
	if budget <= 0 {
		return 0, fmt.Errorf("build context exceeds the %d byte limit", maxBuildContextBytes)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", dest, err)
	}
	defer func() { _ = f.Close() }()
	// Copy one byte past the budget so an over-budget file is detected rather
	// than silently truncated.
	n, err := io.Copy(f, io.LimitReader(tr, budget+1))
	if err != nil {
		return n, fmt.Errorf("write %s: %w", dest, err)
	}
	if n > budget {
		return n, fmt.Errorf("build context exceeds the %d byte limit", maxBuildContextBytes)
	}
	return n, nil
}

// safeJoin joins a cleaned, rooted guest path onto base, guaranteeing the
// result stays within base (defends against path traversal in the tar).
func safeJoin(base, guestPath string) (string, error) {
	dest := filepath.Join(base, filepath.FromSlash(guestPath))
	rel, err := filepath.Rel(base, dest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("build context entry %q escapes context root", guestPath)
	}
	return dest, nil
}

// splitProgram separates the main program file (matched by guest path) from the
// remaining package/source files.
func splitProgram(files []pkg.File, program string) (string, []pkg.File, error) {
	if program == "" {
		return "", nil, fmt.Errorf("build: program path is required")
	}
	want := strings.TrimPrefix(filepath.ToSlash(filepath.Clean("/"+program)), "/")
	var binaryPath string
	pkgFiles := make([]pkg.File, 0, len(files))
	for _, f := range files {
		if f.GuestPath == want {
			binaryPath = f.HostPath
			continue
		}
		pkgFiles = append(pkgFiles, f)
	}
	if binaryPath == "" {
		return "", nil, fmt.Errorf("build: program %q not found in build context", program)
	}
	return binaryPath, pkgFiles, nil
}

// skipLeadingWhitespace discards leading JSON whitespace bytes so the frame
// stream that follows a request starts on its first real byte.
func skipLeadingWhitespace(br *bufio.Reader) {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return
		}
		if b != ' ' && b != '\t' && b != '\n' && b != '\r' {
			_ = br.UnreadByte()
			return
		}
	}
}

// drain consumes and discards any remaining frame stream so a connection stays
// in a clean state after an early error.
func drain(stream io.Reader) {
	_, _ = io.Copy(io.Discard, stream)
}
