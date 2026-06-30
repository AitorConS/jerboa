package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
)

// Client connects to a jerboad server over a Unix socket or TCP.
type Client struct {
	endpoint string
	token    string
	conn     net.Conn
	mu       sync.Mutex
	enc      *json.Encoder
	dec      *json.Decoder
	seq      atomic.Int64
}

// Dial connects to the jerboad server at endpoint. The endpoint may carry a
// scheme (unix:// or tcp://); a bare value is treated as a Unix socket path.
// The auth token is read from the JERBOA_AUTH_TOKEN environment variable.
func Dial(endpoint string) (*Client, error) {
	return DialWithToken(endpoint, os.Getenv("JERBOA_AUTH_TOKEN"))
}

// DialWithToken connects like Dial and, when token is non-empty, performs the
// Auth.Hello handshake before returning. The same token is reused for any
// secondary connection the client opens (e.g. ImageBuild).
func DialWithToken(endpoint, token string) (*Client, error) {
	network, address, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	conn, err := net.Dial(network, address) //nolint:noctx // persistent daemon connection; no request context
	if err != nil {
		return nil, fmt.Errorf("api client dial %s: %w", endpoint, err)
	}
	c := &Client{
		endpoint: endpoint,
		token:    token,
		conn:     conn,
		enc:      json.NewEncoder(conn),
		dec:      json.NewDecoder(conn),
	}
	if token != "" {
		if err := sendAuth(c.enc, c.dec, token); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return c, nil
}

// sendAuth performs the Auth.Hello handshake on a connection's encoder/decoder.
func sendAuth(enc *json.Encoder, dec *json.Decoder, token string) error {
	raw, err := json.Marshal(AuthParams{Token: token})
	if err != nil {
		return fmt.Errorf("marshal auth: %w", err)
	}
	req := Request{JSONRPC: "2.0", ID: 1, Method: "Auth.Hello", Params: json.RawMessage(raw)}
	if err := enc.Encode(req); err != nil {
		return fmt.Errorf("send auth: %w", err)
	}
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		return fmt.Errorf("auth response: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("authentication failed: %s", resp.Error.Message)
	}
	return nil
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("api client close: %w", err)
	}
	return nil
}

// Run creates and starts a VM, returning its info.
func (c *Client) Run(_ context.Context, p RunParams) (VMInfo, error) {
	var info VMInfo
	if err := c.call("VM.Run", p, &info); err != nil {
		return VMInfo{}, fmt.Errorf("client run: %w", err)
	}
	return info, nil
}

// Stop sends a graceful stop request. Set force=true for immediate SIGKILL.
func (c *Client) Stop(_ context.Context, id string, force bool) error {
	if err := c.call("VM.Stop", StopParams{ID: id, Force: force}, nil); err != nil {
		return fmt.Errorf("client stop: %w", err)
	}
	return nil
}

// Kill sends an immediate SIGKILL to the VM.
func (c *Client) Kill(_ context.Context, id string) error {
	if err := c.call("VM.Kill", IDParams{ID: id}, nil); err != nil {
		return fmt.Errorf("client kill: %w", err)
	}
	return nil
}

// Signal sends the named signal to the VM process.
func (c *Client) Signal(_ context.Context, id, sig string) error {
	if err := c.call("VM.Signal", SignalParams{ID: id, Signal: sig}, nil); err != nil {
		return fmt.Errorf("client signal: %w", err)
	}
	return nil
}

// Remove removes the VM with the given id.
func (c *Client) Remove(_ context.Context, id string) error {
	if err := c.call("VM.Remove", IDParams{ID: id}, nil); err != nil {
		return fmt.Errorf("client remove: %w", err)
	}
	return nil
}

// List returns all VMs known to the daemon.
func (c *Client) List(_ context.Context) ([]VMInfo, error) {
	var infos []VMInfo
	if err := c.call("VM.List", nil, &infos); err != nil {
		return nil, fmt.Errorf("client list: %w", err)
	}
	return infos, nil
}

// Get returns the VM with the given id.
func (c *Client) Get(_ context.Context, id string) (VMInfo, error) {
	var info VMInfo
	if err := c.call("VM.Get", IDParams{ID: id}, &info); err != nil {
		return VMInfo{}, fmt.Errorf("client get: %w", err)
	}
	return info, nil
}

// Logs returns captured serial console output for the VM.
func (c *Client) Logs(_ context.Context, id string) (LogsResponse, error) {
	var resp LogsResponse
	if err := c.call("VM.Logs", IDParams{ID: id}, &resp); err != nil {
		return LogsResponse{}, fmt.Errorf("client logs: %w", err)
	}
	return resp, nil
}

// Shutdown asks the daemon to exit cleanly.
func (c *Client) Shutdown(_ context.Context) error {
	if err := c.call("Daemon.Shutdown", nil, nil); err != nil {
		return fmt.Errorf("client shutdown: %w", err)
	}
	return nil
}

// DaemonVersion returns the version string reported by the running daemon.
func (c *Client) DaemonVersion(_ context.Context) (string, error) {
	var resp map[string]string
	if err := c.call("Daemon.Version", nil, &resp); err != nil {
		return "", fmt.Errorf("client daemon version: %w", err)
	}
	return resp["version"], nil
}

// NetworkCreate creates a new network.
func (c *Client) NetworkCreate(_ context.Context, name, subnet, driver string) (NetworkInfo, error) {
	var info NetworkInfo
	p := NetworkCreateParams{Name: name, Subnet: subnet, Driver: driver}
	if err := c.call("Network.Create", p, &info); err != nil {
		return NetworkInfo{}, fmt.Errorf("client network create: %w", err)
	}
	return info, nil
}

// NetworkList returns all networks.
func (c *Client) NetworkList(_ context.Context) ([]NetworkInfo, error) {
	var infos []NetworkInfo
	if err := c.call("Network.List", nil, &infos); err != nil {
		return nil, fmt.Errorf("client network list: %w", err)
	}
	return infos, nil
}

// NetworkGet returns a single network by name.
func (c *Client) NetworkGet(_ context.Context, name string) (NetworkInfo, error) {
	var info NetworkInfo
	if err := c.call("Network.Get", struct {
		Name string `json:"name"`
	}{Name: name}, &info); err != nil {
		return NetworkInfo{}, fmt.Errorf("client network get: %w", err)
	}
	return info, nil
}

// NetworkRemove deletes a network by name.
func (c *Client) NetworkRemove(_ context.Context, name string) error {
	if err := c.call("Network.Remove", struct {
		Name string `json:"name"`
	}{Name: name}, nil); err != nil {
		return fmt.Errorf("client network remove: %w", err)
	}
	return nil
}

// NetworkAllocateIP allocates an IP address from the network's subnet.
func (c *Client) NetworkAllocateIP(_ context.Context, networkName string) (string, error) {
	var resp map[string]string
	if err := c.call("Network.AllocateIP", struct {
		Network string `json:"network"`
	}{Network: networkName}, &resp); err != nil {
		return "", fmt.Errorf("client network allocate ip: %w", err)
	}
	return resp["ip"], nil
}

// NetworkReleaseIP releases an allocated IP address back to the network.
func (c *Client) NetworkReleaseIP(_ context.Context, networkName, ip string) error {
	if err := c.call("Network.ReleaseIP", struct {
		Network string `json:"network"`
		IP      string `json:"ip"`
	}{Network: networkName, IP: ip}, nil); err != nil {
		return fmt.Errorf("client network release ip: %w", err)
	}
	return nil
}

// DNSResolve resolves a VM name to an IP address inside an optional network.
func (c *Client) DNSResolve(_ context.Context, name, network string) (DNSRecord, error) {
	var rec DNSRecord
	p := DNSResolveParams{Name: name, Network: network}
	if err := c.call("DNS.Resolve", p, &rec); err != nil {
		return DNSRecord{}, fmt.Errorf("client dns resolve: %w", err)
	}
	return rec, nil
}

// DNSList lists resolvable VM records, optionally filtered by network.
func (c *Client) DNSList(_ context.Context, network string) ([]DNSRecord, error) {
	var recs []DNSRecord
	p := struct {
		Network string `json:"network,omitempty"`
	}{Network: network}
	if err := c.call("DNS.List", p, &recs); err != nil {
		return nil, fmt.Errorf("client dns list: %w", err)
	}
	return recs, nil
}

// Inspect returns full details for the VM.
func (c *Client) Inspect(_ context.Context, id string) (VMDetail, error) {
	var detail VMDetail
	if err := c.call("VM.Inspect", IDParams{ID: id}, &detail); err != nil {
		return VMDetail{}, fmt.Errorf("client inspect: %w", err)
	}
	return detail, nil
}

// Stats returns runtime resource usage for the VM.
func (c *Client) Stats(_ context.Context, id string) (VMStatsResponse, error) {
	var stats VMStatsResponse
	if err := c.call("VM.Stats", IDParams{ID: id}, &stats); err != nil {
		return VMStatsResponse{}, fmt.Errorf("client stats: %w", err)
	}
	return stats, nil
}

// NodeList returns cluster member information.
func (c *Client) NodeList(_ context.Context) (NodeListResponse, error) {
	var resp NodeListResponse
	if err := c.call("Node.List", nil, &resp); err != nil {
		return NodeListResponse{}, fmt.Errorf("client node list: %w", err)
	}
	return resp, nil
}

// ServiceRun creates and starts a service with the given parameters.
func (c *Client) ServiceRun(_ context.Context, p ServiceRunParams) (ServiceInfoResult, error) {
	var result ServiceInfoResult
	if err := c.call("Service.Run", p, &result); err != nil {
		return ServiceInfoResult{}, fmt.Errorf("client service run: %w", err)
	}
	return result, nil
}

// ServiceScale adjusts the number of replicas for a service.
func (c *Client) ServiceScale(_ context.Context, name string, desiredReplicas int) (ServiceInfoResult, error) {
	var result ServiceInfoResult
	p := ServiceScaleParams{Name: name, DesiredReplicas: desiredReplicas}
	if err := c.call("Service.Scale", p, &result); err != nil {
		return ServiceInfoResult{}, fmt.Errorf("client service scale: %w", err)
	}
	return result, nil
}

// ServiceUpdate performs a rolling update of a service to a new image.
// healthTimeout is the maximum seconds to wait for new replicas to become
// healthy before removing old ones. Zero means no waiting.
func (c *Client) ServiceUpdate(_ context.Context, name, image string, healthTimeout int) (ServiceInfoResult, error) {
	var result ServiceInfoResult
	p := ServiceUpdateParams{Name: name, Image: image, HealthTimeout: healthTimeout}
	if err := c.call("Service.Update", p, &result); err != nil {
		return ServiceInfoResult{}, fmt.Errorf("client service update: %w", err)
	}
	return result, nil
}

// ServiceList returns all services.
func (c *Client) ServiceList(_ context.Context) ([]ServiceInfoResult, error) {
	var result []ServiceInfoResult
	if err := c.call("Service.List", nil, &result); err != nil {
		return nil, fmt.Errorf("client service list: %w", err)
	}
	return result, nil
}

// ServiceGet returns a single service by name.
func (c *Client) ServiceGet(_ context.Context, name string) (ServiceInfoResult, error) {
	var result ServiceInfoResult
	p := struct {
		Name string `json:"name"`
	}{Name: name}
	if err := c.call("Service.Get", p, &result); err != nil {
		return ServiceInfoResult{}, fmt.Errorf("client service get: %w", err)
	}
	return result, nil
}

// ServiceRemove stops all replicas of a service and deletes it.
func (c *Client) ServiceRemove(_ context.Context, name string) error {
	p := struct {
		Name string `json:"name"`
	}{Name: name}
	if err := c.call("Service.Remove", p, nil); err != nil {
		return fmt.Errorf("client service remove: %w", err)
	}
	return nil
}

// DNSResolveAll resolves all DNS records matching a name (round-robin).
func (c *Client) DNSResolveAll(_ context.Context, name, network string) ([]DNSRecord, error) {
	var recs []DNSRecord
	p := DNSResolveParams{Name: name, Network: network}
	if err := c.call("DNS.ResolveAll", p, &recs); err != nil {
		return nil, fmt.Errorf("client dns resolve all: %w", err)
	}
	return recs, nil
}

// Attach connects to a VM's serial console and streams output to stdout.
// It blocks until the VM stops or the connection is closed.
// This method takes over the connection for raw reading; do not use the
// client for other calls after Attach.
func (c *Client) Attach(_ context.Context, id string, out io.Writer) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	reqID := c.seq.Add(1)
	params, _ := json.Marshal(IDParams{ID: id})
	req := Request{
		JSONRPC: "2.0",
		ID:      reqID,
		Method:  "VM.Attach",
		Params:  json.RawMessage(params),
	}
	if err := c.enc.Encode(req); err != nil {
		return fmt.Errorf("encode attach request: %w", err)
	}

	var resp Response
	if err := c.dec.Decode(&resp); err != nil {
		return fmt.Errorf("decode attach response: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
	}

	buf := make([]byte, 4096)
	for {
		n, err := c.conn.Read(buf)
		if n > 0 {
			if _, writeErr := out.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("write attach output: %w", writeErr)
			}
		}
		if err != nil {
			return nil
		}
	}
}

// ImageBuild sends an Image.Build request and streams the build context to the
// daemon, which assembles the disk image with mkfs on its own filesystem and
// stores it. contextTar is an uncompressed tar archive containing the program
// binary (at p.Program) plus any package or source files.
//
// Build uses a dedicated connection (the daemon consumes the streamed context
// and closes that connection), leaving the client's persistent connection free
// for concurrent calls.
func (c *Client) ImageBuild(_ context.Context, p BuildParams, contextTar io.Reader) (ImageManifestResult, error) {
	conn, err := net.Dial(dialArgs(c.endpoint)) //nolint:noctx // dedicated build connection
	if err != nil {
		return ImageManifestResult{}, fmt.Errorf("dial build connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	if c.token != "" {
		if err := sendAuth(enc, dec, c.token); err != nil {
			return ImageManifestResult{}, err
		}
	}

	raw, err := json.Marshal(p)
	if err != nil {
		return ImageManifestResult{}, fmt.Errorf("marshal build params: %w", err)
	}
	req := Request{JSONRPC: "2.0", ID: 1, Method: "Image.Build", Params: json.RawMessage(raw)}
	if err := enc.Encode(req); err != nil {
		return ImageManifestResult{}, fmt.Errorf("encode build request: %w", err)
	}

	fw := NewFrameWriter(conn)
	if _, err := io.Copy(fw, contextTar); err != nil {
		return ImageManifestResult{}, fmt.Errorf("stream build context: %w", err)
	}
	if err := fw.Close(); err != nil {
		return ImageManifestResult{}, fmt.Errorf("finish build context: %w", err)
	}

	var resp Response
	if err := dec.Decode(&resp); err != nil {
		return ImageManifestResult{}, fmt.Errorf("decode build response: %w", err)
	}
	if resp.Error != nil {
		return ImageManifestResult{}, fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	var out ImageManifestResult
	if resp.Result != nil {
		if err := json.Unmarshal(resp.Result, &out); err != nil {
			return ImageManifestResult{}, fmt.Errorf("unmarshal build result: %w", err)
		}
	}
	return out, nil
}

// VolumeSeed populates a volume's disk with an initialized filesystem built
// from the streamed context tar. It mirrors ImageBuild's framed streaming: the
// request is sent, then the tar bytes are framed onto the same connection, then
// the daemon's response is read. A dedicated connection is used and closed after.
func (c *Client) VolumeSeed(_ context.Context, p VolumeSeedParams, contextTar io.Reader) (VolumeSeedResult, error) {
	conn, err := net.Dial(dialArgs(c.endpoint)) //nolint:noctx // dedicated seed connection
	if err != nil {
		return VolumeSeedResult{}, fmt.Errorf("dial seed connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	if c.token != "" {
		if err := sendAuth(enc, dec, c.token); err != nil {
			return VolumeSeedResult{}, err
		}
	}

	raw, err := json.Marshal(p)
	if err != nil {
		return VolumeSeedResult{}, fmt.Errorf("marshal seed params: %w", err)
	}
	req := Request{JSONRPC: "2.0", ID: 1, Method: "Volume.Seed", Params: json.RawMessage(raw)}
	if err := enc.Encode(req); err != nil {
		return VolumeSeedResult{}, fmt.Errorf("encode seed request: %w", err)
	}

	fw := NewFrameWriter(conn)
	if _, err := io.Copy(fw, contextTar); err != nil {
		return VolumeSeedResult{}, fmt.Errorf("stream seed context: %w", err)
	}
	if err := fw.Close(); err != nil {
		return VolumeSeedResult{}, fmt.Errorf("finish seed context: %w", err)
	}

	var resp Response
	if err := dec.Decode(&resp); err != nil {
		return VolumeSeedResult{}, fmt.Errorf("decode seed response: %w", err)
	}
	if resp.Error != nil {
		return VolumeSeedResult{}, fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	var out VolumeSeedResult
	if resp.Result != nil {
		if err := json.Unmarshal(resp.Result, &out); err != nil {
			return VolumeSeedResult{}, fmt.Errorf("unmarshal seed result: %w", err)
		}
	}
	return out, nil
}

// ImageList returns the images held in the daemon's store.
func (c *Client) ImageList(_ context.Context) ([]ImageManifestResult, error) {
	var out []ImageManifestResult
	if err := c.call("Image.List", nil, &out); err != nil {
		return nil, fmt.Errorf("client image list: %w", err)
	}
	return out, nil
}

// ImageGet returns the manifest for a single name:tag (or sha) reference.
func (c *Client) ImageGet(_ context.Context, ref string) (ImageManifestResult, error) {
	var out ImageManifestResult
	p := struct {
		Ref string `json:"ref"`
	}{Ref: ref}
	if err := c.call("Image.Get", p, &out); err != nil {
		return ImageManifestResult{}, fmt.Errorf("client image get: %w", err)
	}
	return out, nil
}

// ImageRemove deletes a name:tag (or sha) reference from the daemon's store.
func (c *Client) ImageRemove(_ context.Context, ref string) error {
	p := struct {
		Ref string `json:"ref"`
	}{Ref: ref}
	if err := c.call("Image.Remove", p, nil); err != nil {
		return fmt.Errorf("client image remove: %w", err)
	}
	return nil
}

func (c *Client) call(method string, params any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.seq.Add(1)
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal params: %w", err)
	}
	req := Request{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  json.RawMessage(raw),
	}
	if err := c.enc.Encode(req); err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	var resp Response
	if err := c.dec.Decode(&resp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	if out != nil && resp.Result != nil {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("unmarshal result: %w", err)
		}
	}
	return nil
}
