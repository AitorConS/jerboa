package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/image"
	"github.com/AitorConS/unikernel-engine/internal/ociregistry"
)

// Client pushes and pulls images to/from a registry Server.
type Client struct {
	baseURL string
	http    *http.Client
	token   string
}

// NewClient returns a Client targeting the registry at baseURL (e.g. "http://localhost:5000").
func NewClient(baseURL string) *Client {
	return &Client{baseURL: baseURL, http: &http.Client{}}
}

// SetToken configures a bearer token for registry requests.
func (c *Client) SetToken(token string) {
	c.token = token
}

// PushOCI uploads an image using OCI blob and manifest endpoints.
func (c *Client) PushOCI(ctx context.Context, m image.Manifest, diskPath string) error {
	config := ociregistry.Config{Memory: m.Config.Memory, CPUs: m.Config.CPUs, Env: m.Config.Env, Created: m.Created.UTC().Format(time.RFC3339)}
	configJSON, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("registry OCI push: marshal config: %w", err)
	}
	configDesc, err := c.uploadBlob(ctx, m.Name, bytes.NewReader(configJSON), ociregistry.MediaTypeImageConfig)
	if err != nil {
		return fmt.Errorf("registry OCI push: upload config: %w", err)
	}

	layer, err := packDiskLayer(diskPath)
	if err != nil {
		return fmt.Errorf("registry OCI push: pack layer: %w", err)
	}
	layerDesc, err := c.uploadBlob(ctx, m.Name, bytes.NewReader(layer), ociregistry.MediaTypeImageLayerTarGzip)
	if err != nil {
		return fmt.Errorf("registry OCI push: upload layer: %w", err)
	}

	manifest := ociregistry.Manifest{
		SchemaVersion: ociregistry.OCIManifestSchemaVersion,
		MediaType:     ociregistry.MediaTypeImageManifest,
		Config:        configDesc,
		Layers:        []ociregistry.Descriptor{layerDesc},
		Annotations:   map[string]string{"org.opencontainers.image.ref.name": m.Tag},
	}
	body, err := ociregistry.MarshalManifest(manifest)
	if err != nil {
		return fmt.Errorf("registry OCI push: marshal manifest: %w", err)
	}

	url := fmt.Sprintf("%s/v2/%s/manifests/%s", c.baseURL, m.Name, m.Tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("registry OCI push: build request: %w", err)
	}
	req.Header.Set("Content-Type", ociregistry.MediaTypeImageManifest)
	c.addAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("registry OCI push: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("registry OCI push: server returned %d: %s", resp.StatusCode, msg)
	}
	return nil
}

// PullOCI pulls an image through OCI endpoints and stores it in legacy image store.
func (c *Client) PullOCI(ctx context.Context, ref string, store *image.Store) (image.Manifest, error) {
	name, tag := splitRef(ref)
	if name == "" || tag == "" {
		return image.Manifest{}, fmt.Errorf("registry OCI pull: invalid ref %q", ref)
	}

	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", c.baseURL, name, tag)
	manifestReq, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return image.Manifest{}, fmt.Errorf("registry OCI pull: build manifest request: %w", err)
	}
	c.addAuth(manifestReq)
	manifestResp, err := c.http.Do(manifestReq)
	if err != nil {
		return image.Manifest{}, fmt.Errorf("registry OCI pull: get manifest: %w", err)
	}
	defer func() { _ = manifestResp.Body.Close() }()
	if manifestResp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(manifestResp.Body)
		return image.Manifest{}, fmt.Errorf("registry OCI pull: get manifest returned %d: %s", manifestResp.StatusCode, msg)
	}
	body, err := io.ReadAll(manifestResp.Body)
	if err != nil {
		return image.Manifest{}, fmt.Errorf("registry OCI pull: read manifest: %w", err)
	}
	manifest, err := ociregistry.ParseManifest(body)
	if err != nil {
		return image.Manifest{}, fmt.Errorf("registry OCI pull: parse manifest: %w", err)
	}
	if len(manifest.Layers) == 0 {
		return image.Manifest{}, fmt.Errorf("registry OCI pull: manifest has no layers")
	}

	layerPath, err := c.downloadBlob(ctx, name, manifest.Layers[0].Digest)
	if err != nil {
		return image.Manifest{}, fmt.Errorf("registry OCI pull: download layer: %w", err)
	}
	defer func() { _ = os.Remove(layerPath) }()

	diskPath, err := unpackDiskLayer(layerPath)
	if err != nil {
		return image.Manifest{}, fmt.Errorf("registry OCI pull: unpack layer: %w", err)
	}
	defer func() { _ = os.Remove(diskPath) }()

	imgManifest := image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Name:          name,
		Tag:           tag,
		Created:       time.Now().UTC(),
		Config:        image.Config{Memory: "256M", CPUs: 1},
		DiskDigest:    manifest.Layers[0].Digest,
	}
	st, err := os.Stat(diskPath)
	if err != nil {
		return image.Manifest{}, fmt.Errorf("registry OCI pull: stat disk: %w", err)
	}
	imgManifest.DiskSize = st.Size()
	if err := store.Put(name, tag, imgManifest, diskPath); err != nil {
		return image.Manifest{}, fmt.Errorf("registry OCI pull: store: %w", err)
	}

	stored, _, err := store.Get(ref)
	if err != nil {
		return image.Manifest{}, fmt.Errorf("registry OCI pull: get stored image: %w", err)
	}
	return stored, nil
}

// Push uploads a manifest and its disk image to the registry.
func (c *Client) Push(ctx context.Context, m image.Manifest, diskPath string) error {
	manifestJSON, err := image.Marshal(m)
	if err != nil {
		return fmt.Errorf("registry push: marshal manifest: %w", err)
	}
	body, ct, err := buildMultipart(manifestJSON, diskPath)
	if err != nil {
		return fmt.Errorf("registry push: build multipart: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v2/images", body)
	if err != nil {
		return fmt.Errorf("registry push: build request: %w", err)
	}
	req.Header.Set("Content-Type", ct)
	c.addAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("registry push: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Warn("registry client: close push body", "err", err)
		}
	}()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("registry push: server returned %d: %s", resp.StatusCode, msg)
	}
	return nil
}

// Pull downloads the manifest and disk image for ref and stores them in store.
func (c *Client) Pull(ctx context.Context, ref string, store *image.Store) (image.Manifest, error) {
	m, err := c.getManifest(ctx, ref)
	if err != nil {
		return image.Manifest{}, fmt.Errorf("registry pull %s: %w", ref, err)
	}
	diskPath, err := c.getDisk(ctx, ref)
	if err != nil {
		return image.Manifest{}, fmt.Errorf("registry pull %s: %w", ref, err)
	}
	defer func() { _ = os.Remove(diskPath) }()

	if err := store.Put(m.Name, m.Tag, m, diskPath); err != nil {
		return image.Manifest{}, fmt.Errorf("registry pull %s: store: %w", ref, err)
	}
	return m, nil
}

// List returns all manifests known to the registry.
func (c *Client) List(ctx context.Context) ([]image.Manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v2/images", nil)
	if err != nil {
		return nil, fmt.Errorf("registry list: build request: %w", err)
	}
	c.addAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry list: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Warn("registry client: close list body", "err", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry list: server returned %d", resp.StatusCode)
	}
	var out []image.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("registry list: decode: %w", err)
	}
	return out, nil
}

func (c *Client) getManifest(ctx context.Context, ref string) (image.Manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v2/images/"+ref, nil)
	if err != nil {
		return image.Manifest{}, fmt.Errorf("get manifest: build request: %w", err)
	}
	c.addAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return image.Manifest{}, fmt.Errorf("get manifest: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Warn("registry client: close manifest body", "err", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return image.Manifest{}, fmt.Errorf("get manifest: server returned %d", resp.StatusCode)
	}
	var m image.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return image.Manifest{}, fmt.Errorf("get manifest: decode: %w", err)
	}
	return m, nil
}

func (c *Client) getDisk(ctx context.Context, ref string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v2/images/"+ref+"/disk", nil)
	if err != nil {
		return "", fmt.Errorf("get disk: build request: %w", err)
	}
	c.addAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("get disk: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Warn("registry client: close disk body", "err", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get disk: server returned %d", resp.StatusCode)
	}
	f, err := os.CreateTemp("", "uni-pull-*.img")
	if err != nil {
		return "", fmt.Errorf("get disk: create temp: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			_ = err // best effort
		}
	}()
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("get disk: write temp: %w", err)
	}
	return f.Name(), nil
}

func buildMultipart(manifestJSON []byte, diskPath string) (io.Reader, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	if err := w.WriteField("manifest", string(manifestJSON)); err != nil {
		return nil, "", fmt.Errorf("write manifest field: %w", err)
	}
	fw, err := w.CreateFormFile("disk", "disk.img")
	if err != nil {
		return nil, "", fmt.Errorf("create disk field: %w", err)
	}
	f, err := os.Open(diskPath)
	if err != nil {
		return nil, "", fmt.Errorf("open disk %s: %w", diskPath, err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			_ = err // best effort
		}
	}()
	if _, err := io.Copy(fw, f); err != nil {
		return nil, "", fmt.Errorf("copy disk: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart writer: %w", err)
	}
	return &buf, w.FormDataContentType(), nil
}

func (c *Client) uploadBlob(ctx context.Context, name string, r io.Reader, mediaType string) (ociregistry.Descriptor, error) {
	startURL := fmt.Sprintf("%s/v2/%s/blobs/uploads/", c.baseURL, name)
	startReq, err := http.NewRequestWithContext(ctx, http.MethodPost, startURL, nil)
	if err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("start upload request: %w", err)
	}
	c.addAuth(startReq)
	startResp, err := c.http.Do(startReq)
	if err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("start upload: %w", err)
	}
	defer func() { _ = startResp.Body.Close() }()
	if startResp.StatusCode != http.StatusAccepted {
		msg, _ := io.ReadAll(startResp.Body)
		return ociregistry.Descriptor{}, fmt.Errorf("start upload returned %d: %s", startResp.StatusCode, msg)
	}
	location := startResp.Header.Get("Location")
	if location == "" {
		return ociregistry.Descriptor{}, fmt.Errorf("start upload missing Location header")
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("read upload body: %w", err)
	}
	digest := image.DigestSHA256(data)
	completeURL := c.baseURL + location + "?digest=" + digest
	completeReq, err := http.NewRequestWithContext(ctx, http.MethodPut, completeURL, bytes.NewReader(data))
	if err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("complete upload request: %w", err)
	}
	c.addAuth(completeReq)
	completeResp, err := c.http.Do(completeReq)
	if err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("complete upload: %w", err)
	}
	defer func() { _ = completeResp.Body.Close() }()
	if completeResp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(completeResp.Body)
		return ociregistry.Descriptor{}, fmt.Errorf("complete upload returned %d: %s", completeResp.StatusCode, msg)
	}

	return ociregistry.Descriptor{MediaType: mediaType, Digest: digest, Size: int64(len(data))}, nil
}

func packDiskLayer(diskPath string) ([]byte, error) {
	f, err := os.Open(diskPath)
	if err != nil {
		return nil, fmt.Errorf("open disk: %w", err)
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat disk: %w", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	header := &tar.Header{Name: filepath.Base(diskPath), Mode: 0o644, Size: st.Size()}
	if err := tw.WriteHeader(header); err != nil {
		return nil, fmt.Errorf("write layer header: %w", err)
	}
	if _, err := io.Copy(tw, f); err != nil {
		return nil, fmt.Errorf("write layer contents: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close tar writer: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("close gzip writer: %w", err)
	}
	return buf.Bytes(), nil
}

func unpackDiskLayer(layerPath string) (string, error) {
	f, err := os.Open(layerPath)
	if err != nil {
		return "", fmt.Errorf("open layer: %w", err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("open gzip layer: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	header, err := tr.Next()
	if err != nil {
		return "", fmt.Errorf("read layer header: %w", err)
	}
	if header.FileInfo().IsDir() {
		return "", fmt.Errorf("invalid layer: first entry is directory")
	}

	out, err := os.CreateTemp("", "uni-oci-disk-*.img")
	if err != nil {
		return "", fmt.Errorf("create disk temp: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(out.Name())
		}
	}()
	if _, err := io.Copy(out, tr); err != nil {
		_ = out.Close()
		return "", fmt.Errorf("write disk temp: %w", err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("close disk temp: %w", err)
	}
	return out.Name(), nil
}

func (c *Client) downloadBlob(ctx context.Context, name, digest string) (string, error) {
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", c.baseURL, name, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build blob request: %w", err)
	}
	c.addAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("download blob: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("download blob returned %d: %s", resp.StatusCode, msg)
	}
	file, err := os.CreateTemp("", "uni-oci-layer-*.tgz")
	if err != nil {
		return "", fmt.Errorf("create blob temp: %w", err)
	}
	if _, err := io.Copy(file, resp.Body); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return "", fmt.Errorf("write blob temp: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close blob temp: %w", err)
	}
	return file.Name(), nil
}

func splitRef(ref string) (string, string) {
	idx := strings.LastIndex(ref, ":")
	if idx <= 0 || idx >= len(ref)-1 {
		return "", ""
	}
	return ref[:idx], ref[idx+1:]
}

func (c *Client) addAuth(req *http.Request) {
	if strings.TrimSpace(c.token) == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
}
