package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/image"
	"github.com/AitorConS/jerboa/internal/naming"
	"github.com/AitorConS/jerboa/internal/registry/client"
	"github.com/AitorConS/jerboa/internal/registry/transfer"
)

// EnableRegistry turns on Image.Push and Image.Pull. mkfsFor returns the mkfs
// of a toolset version: pushes assemble the image once before uploading, and
// pulls assemble the disk from the recipe.
func (s *Server) EnableRegistry(mkfsFor transfer.MkfsFor) {
	s.registryMkfs = mkfsFor
}

func (s *Server) registryEnabled() *api.RPCError {
	if s.imgStore == nil || s.recipes == nil || s.registryMkfs == nil {
		return &api.RPCError{Code: -32601, Message: "registry push and pull are disabled on this daemon"}
	}
	return nil
}

func rpcErr(err error) *api.RPCError { return &api.RPCError{Code: -32000, Message: err.Error()} }

func (s *Server) handleImageTag(params json.RawMessage) (any, *api.RPCError) {
	if s.imgStore == nil {
		return nil, &api.RPCError{Code: -32601, Message: "method not found: Image.Tag (image store disabled)"}
	}
	var p api.ImageTagParams
	if err := json.Unmarshal(params, &p); err != nil || p.Source == "" || p.Name == "" || p.Tag == "" {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: source, name and tag are required"}
	}
	m, err := s.imgStore.Tag(p.Source, p.Name, p.Tag)
	if err != nil {
		return nil, rpcErr(err)
	}
	return imageManifestResult(m), nil
}

func (s *Server) handleImagePush(ctx context.Context, params json.RawMessage) (any, *api.RPCError) {
	if e := s.registryEnabled(); e != nil {
		return nil, e
	}
	var p api.ImagePushParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	ref, err := naming.ParseImageReference(p.Reference)
	if err != nil {
		return nil, rpcErr(err)
	}
	if ref.Digest != "" {
		return nil, rpcErr(fmt.Errorf("push needs a tag, not a digest"))
	}
	local := ref.Name() + ":" + ref.Tag
	m, _, err := s.imgStore.Resolve(local)
	if err != nil {
		return nil, rpcErr(fmt.Errorf("%s is not in the image store; jerboa tag an image with this name first", local))
	}
	if m.Recipe == "" {
		return nil, rpcErr(fmt.Errorf("%s has no recipe; rebuild it with this daemon", local))
	}
	res, err := transfer.Push(ctx, transfer.PushOptions{
		Client: client.New(ref.Registry), Token: p.Token, Ref: ref,
		Store: s.recipes, Recipe: m.Recipe, Mkfs: s.registryMkfs,
	})
	if err != nil {
		return nil, rpcErr(err)
	}
	return api.ImagePushResult{
		Reference: ref.String(), Recipe: res.Recipe, Blobs: res.Blobs,
		BlobsUploaded: res.BlobsUploaded, PacksUploaded: res.PacksUploaded, BytesUploaded: res.BytesUploaded,
	}, nil
}

func (s *Server) handleImagePull(ctx context.Context, params json.RawMessage) (any, *api.RPCError) {
	if e := s.registryEnabled(); e != nil {
		return nil, e
	}
	var p api.ImagePullParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	ref, err := naming.ParseImageReference(p.Reference)
	if err != nil {
		return nil, rpcErr(err)
	}
	out, err := s.imgStore.TempFile(".pull-*.img")
	if err != nil {
		return nil, rpcErr(err)
	}
	defer func() { _ = os.Remove(out) }()
	res, err := transfer.Pull(ctx, transfer.PullOptions{
		Client: client.New(ref.Registry), Ref: ref, Store: s.recipes, Mkfs: s.registryMkfs, Out: out,
		NoCount: p.NoCount || transfer.DoNotTrack(),
	})
	if err != nil {
		return nil, rpcErr(err)
	}
	r := res.Parsed
	tag := ref.Tag
	if tag == "" {
		tag = strings.TrimPrefix(res.Recipe, "sha256:")[:12]
	}
	m, err := s.imgStore.PutMove(ref.Name(), tag, image.Manifest{
		SchemaVersion: image.SchemaVersion,
		Platform:      r.Platform,
		Architecture:  r.Architecture,
		Packages:      r.Packages,
		Config:        r.Config,
		Layout:        r.Layout,
		Created:       time.Now().UTC(),
		DiskSize:      r.DiskSize,
		Recipe:        res.Recipe,
	}, out)
	if err != nil {
		return nil, rpcErr(err)
	}
	if m.DiskDigest != r.DiskDigest {
		return nil, rpcErr(fmt.Errorf("stored disk %s differs from the recipe's %s", m.DiskDigest, r.DiskDigest))
	}
	return api.ImagePullResult{
		Image: imageManifestResult(m), Recipe: res.Recipe, Blobs: res.Blobs,
		BlobsFetched: res.BlobsFetched, Requests: res.Requests, BytesFetched: res.BytesFetched,
	}, nil
}
