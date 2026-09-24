package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"

	"github.com/AitorConS/jerboa/internal/volume"
)

// CodeNotFound is returned when a requested resource does not exist.
const CodeNotFound = -32004

// IsNotFound identifies a missing resource without interpreting server messages.
func IsNotFound(err error) bool {
	var rpc *RPCError
	return errors.As(err, &rpc) && rpc.Code == CodeNotFound
}

type VolumeCreateParams struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

type VolumeImportParams struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	SizeBytes int64  `json:"size_bytes"`
}

func (c *Client) VolumeImport(ctx context.Context, p VolumeImportParams, disk io.Reader) (*volume.Volume, error) {
	network, address, err := parseEndpoint(c.endpoint)
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
	if c.token != "" {
		if err := sendAuth(enc, dec, c.token); err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	if err := enc.Encode(Request{JSONRPC: "2.0", ID: 1, Method: "Volume.Import", Params: raw}); err != nil {
		return nil, err
	}
	w := NewFrameWriter(conn)
	if _, err := io.Copy(w, disk); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	var response Response
	if err := dec.Decode(&response); err != nil {
		return nil, err
	}
	if response.Error != nil {
		return nil, response.Error
	}
	var result volume.Volume
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) VolumeCreate(_ context.Context, name string, size int64) (*volume.Volume, error) {
	var result volume.Volume
	err := c.call("Volume.Create", VolumeCreateParams{Name: name, SizeBytes: size}, &result)
	return &result, err
}

func (c *Client) VolumeGet(_ context.Context, name string) (*volume.Volume, error) {
	var result volume.Volume
	err := c.call("Volume.Get", VolumeRemoveParams{Name: name}, &result)
	return &result, err
}

func (c *Client) VolumeList(_ context.Context) ([]*volume.Volume, error) {
	var result []*volume.Volume
	err := c.call("Volume.List", nil, &result)
	return result, err
}
