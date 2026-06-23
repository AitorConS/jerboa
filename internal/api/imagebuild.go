package api

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/AitorConS/unikernel-engine/internal/image"
	pkg "github.com/AitorConS/unikernel-engine/internal/package"
)

// EnableImageBuild turns on the Image.Build RPC. store receives built images
// and mkfs assembles disk images on the daemon's (Linux) filesystem. Until this
// is called, Image.Build reports "method not found". Call once after NewServer.
func (s *Server) EnableImageBuild(store *image.Store, mkfs image.MkfsFunc) {
	s.imgStore = store
	s.mkfs = mkfs
}

// handleBuild reads a streamed build context, assembles a disk image with mkfs
// on the local filesystem, and stores it. The response (or error) is written
// directly to conn; the connection is consumed and not reused afterwards.
func (s *Server) handleBuild(ctx context.Context, params json.RawMessage, stream io.Reader, conn net.Conn, reqID int64) {
	var p BuildParams
	if err := json.Unmarshal(params, &p); err != nil {
		drain(stream)
		s.writeError(conn, reqID, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()})
		return
	}
	if s.imgStore == nil || s.mkfs == nil {
		drain(stream)
		s.writeError(conn, reqID, &RPCError{Code: -32601, Message: "method not found: Image.Build (image build disabled)"})
		return
	}

	tmpDir, err := os.MkdirTemp("", "uni-build-ctx-*")
	if err != nil {
		drain(stream)
		s.writeError(conn, reqID, &RPCError{Code: -32000, Message: "create build context dir: " + err.Error()})
		return
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	files, err := extractBuildContext(stream, tmpDir)
	if err != nil {
		s.writeError(conn, reqID, &RPCError{Code: -32000, Message: "extract build context: " + err.Error()})
		return
	}

	binaryPath, pkgFiles, err := splitProgram(files, p.Program)
	if err != nil {
		s.writeError(conn, reqID, &RPCError{Code: -32602, Message: err.Error()})
		return
	}

	m, err := image.NewBuilder(s.imgStore).Build(ctx, image.BuildConfig{
		Name:       p.Name,
		Tag:        p.Tag,
		BinaryPath: binaryPath,
		MkfsRun:    s.mkfs,
		Memory:     p.Memory,
		CPUs:       p.CPUs,
		PkgFiles:   pkgFiles,
		Entrypoint: p.Entrypoint,
		Args:       p.Args,
		Env:        p.Env,
		Port:       p.Port,
		Output:     io.Discard,
	})
	if err != nil {
		s.writeError(conn, reqID, &RPCError{Code: -32000, Message: err.Error()})
		return
	}

	resp := Response{JSONRPC: "2.0", ID: reqID}
	raw, mErr := json.Marshal(ImageManifestResult{
		Name:       m.Name,
		Tag:        m.Tag,
		DiskDigest: m.DiskDigest,
		DiskSize:   m.DiskSize,
		Created:    m.Created.UTC().Format("2006-01-02T15:04:05Z07:00"),
	})
	if mErr != nil {
		s.writeError(conn, reqID, &RPCError{Code: -32000, Message: "marshal result: " + mErr.Error()})
		return
	}
	resp.Result = raw
	_ = json.NewEncoder(conn).Encode(resp)
}

// extractBuildContext untars stream into dir and returns the extracted files as
// pkg.File entries (HostPath = on-disk location, GuestPath = tar path). It
// rejects entries that escape dir.
func extractBuildContext(stream io.Reader, dir string) ([]pkg.File, error) {
	tr := tar.NewReader(stream)
	var files []pkg.File
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		guestPath := filepath.ToSlash(filepath.Clean("/" + hdr.Name))
		dest, err := safeJoin(dir, guestPath)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir for %s: %w", hdr.Name, err)
		}
		mode := os.FileMode(hdr.Mode).Perm() //nolint:gosec // tar mode bits are bounded by Perm()
		if err := writeFileFromTar(dest, tr, mode); err != nil {
			return nil, err
		}
		files = append(files, pkg.File{HostPath: dest, GuestPath: strings.TrimPrefix(guestPath, "/")})
	}
	return files, nil
}

func writeFileFromTar(dest string, tr io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, tr); err != nil { //nolint:gosec // size bounded by frameReader cap and tar header
		return fmt.Errorf("write %s: %w", dest, err)
	}
	return nil
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
