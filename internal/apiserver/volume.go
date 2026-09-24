//go:build linux || (darwin && arm64)

package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"

	"github.com/AitorConS/jerboa/internal/api"
)

func volumeRPCError(err error) *api.RPCError {
	code := -32000
	if errors.Is(err, os.ErrNotExist) {
		code = api.CodeNotFound
	}
	return &api.RPCError{Code: code, Message: err.Error()}
}

func (s *Server) handleVolumeImport(ctx context.Context, raw json.RawMessage, stream io.Reader, conn net.Conn, id int64) {
	var p api.VolumeImportParams
	if err := json.Unmarshal(raw, &p); err != nil || p.SizeBytes <= 0 {
		drain(stream)
		s.writeError(conn, id, &api.RPCError{Code: -32602, Message: "invalid volume import parameters"})
		return
	}
	if s.volStore == nil {
		drain(stream)
		s.writeError(conn, id, &api.RPCError{Code: -32601, Message: "volume store disabled"})
		return
	}
	v, err := s.volStore.Import(ctx, p.Name, p.Label, p.SizeBytes, stream)
	drain(stream)
	if err != nil {
		s.writeError(conn, id, &api.RPCError{Code: -32000, Message: err.Error()})
		return
	}
	result, err := json.Marshal(v)
	if err != nil {
		s.writeError(conn, id, &api.RPCError{Code: -32000, Message: err.Error()})
		return
	}
	_ = json.NewEncoder(conn).Encode(api.Response{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) handleVolume(method string, raw json.RawMessage) (any, *api.RPCError) {
	if s.volStore == nil {
		return nil, &api.RPCError{Code: -32601, Message: "volume store disabled"}
	}
	var p api.VolumeCreateParams
	if method != "Volume.List" {
		if err := json.Unmarshal(raw, &p); err != nil || p.Name == "" || p.SizeBytes < 0 {
			return nil, &api.RPCError{Code: -32602, Message: "invalid volume parameters"}
		}
	}
	var result any
	var err error
	switch method {
	case "Volume.Create":
		result, err = s.volStore.Create(p.Name, p.SizeBytes)
	case "Volume.Get":
		result, err = s.volStore.Get(p.Name)
	case "Volume.List":
		result, err = s.volStore.List()
	}
	if err != nil {
		return nil, volumeRPCError(err)
	}
	return result, nil
}

func (s *Server) resolveNamedVolumes(mounts []api.VolumeMountSpec) *api.RPCError {
	for i := range mounts {
		m := &mounts[i]
		if m.Name == "" {
			continue
		} // Compatibility with clients that supply host paths.
		if s.volStore == nil {
			return &api.RPCError{Code: -32000, Message: "volume store disabled"}
		}
		v, err := s.volStore.Get(m.Name)
		if err != nil {
			return volumeRPCError(err)
		}
		m.DiskPath, m.Label = v.DiskPath, v.Label
	}
	return nil
}
