package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AitorConS/jerboa/internal/config"
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
	// broken is why the connection can carry no further call: a call was
	// abandoned halfway, so what the daemon sends next answers that one.
	broken error
}

// handshakeTimeout bounds connecting to the daemon and authenticating with
// it. Both are answered at once by a live daemon, so an endpoint that accepts
// and stays silent is not jerboad, or is a jerboad that is stuck.
var handshakeTimeout = 15 * time.Second

// deadlinePast is a deadline in the past: setting it on a connection
// fails whatever is reading from or writing to it.
var deadlinePast = time.Unix(1, 0)

// dialDaemon connects to the daemon without waiting longer than the
// handshake allows.
func dialDaemon(network, address string) (net.Conn, error) {
	d := net.Dialer{Timeout: handshakeTimeout}
	return d.Dial(network, address) //nolint:noctx,wrapcheck // bounded by the dialer timeout; callers name the connection
}

// authenticate runs sendAuth under the handshake deadline, so a silent
// endpoint fails the dial instead of blocking it forever.
func authenticate(conn net.Conn, enc *json.Encoder, dec *json.Decoder, token string) error {
	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return fmt.Errorf("set handshake deadline: %w", err)
	}
	if err := sendAuth(enc, dec, token); err != nil {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return fmt.Errorf("the daemon did not answer the handshake within %s: %w", handshakeTimeout, err)
		}
		return err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear handshake deadline: %w", err)
	}
	return nil
}

// abortOn fails the I/O on conn once ctx ends. The returned function stops
// the watch and reports whether ctx ended first; when it did, conn is left
// with a deadline in the past.
func abortOn(ctx context.Context, conn net.Conn) func() bool {
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(fired)
		_ = conn.SetDeadline(deadlinePast)
	})
	return func() bool {
		if stop() {
			return false
		}
		<-fired
		return true
	}
}

// ended puts the context's own error in front of the I/O error its end
// caused, so callers see context.Canceled rather than a timeout.
func ended(ctx context.Context, err error) error {
	if err == nil || ctx.Err() == nil {
		return err
	}
	return fmt.Errorf("%w (%v)", ctx.Err(), err) //nolint:errorlint // the I/O error is only detail
}

// Dial connects to the jerboad server at endpoint. The endpoint may carry a
// scheme (unix:// or tcp://); a bare value is treated as a Unix socket path.
// The auth token is read from the JERBOA_AUTH_TOKEN environment variable, or
// else the process's session token (config.SetSessionToken).
func Dial(endpoint string) (*Client, error) {
	token := os.Getenv("JERBOA_AUTH_TOKEN")
	if token == "" {
		token = config.SessionToken()
	}
	return DialWithToken(endpoint, token)
}

// DialWithToken connects like Dial and, when token is non-empty, performs the
// Auth.Hello handshake before returning. The same token is reused for any
// secondary connection the client opens (e.g. ImageBuild).
func DialWithToken(endpoint, token string) (*Client, error) {
	network, address, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	conn, err := dialDaemon(network, address)
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
		if err := authenticate(conn, c.enc, c.dec, token); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return c, nil
}

// sendAuth authenticates a connection both ways: it asks the daemon for a
// challenge and answers it with an HMAC of the token, so the token itself is
// never sent, and it requires the daemon to answer the client's own nonce the
// same way before any request goes out. A process squatting the daemon's port,
// or anyone reading the connection, learns nothing reusable and receives no
// request. The Auth.Hello carries this build's wire protocol version so the
// daemon can reject a mismatch, and the reply's proto catches the reverse case
// (a newer client talking to an older daemon).
func sendAuth(enc *json.Encoder, dec *json.Decoder, token string) error {
	var challenge ChallengeResult
	if err := authCall(enc, dec, 1, "Auth.Challenge", struct{}{}, &challenge); err != nil {
		// A daemon from before challenge authentication answers the
		// challenge like any unauthenticated call; anything else (busy,
		// a dropped connection) is reported as it is.
		var rpcErr *authError
		if errors.As(err, &rpcErr) && rpcErr.code == authRequiredCode {
			return fmt.Errorf("%w (a daemon this old does not support challenge authentication; update jerboad)", err)
		}
		return err
	}
	if len(challenge.Nonce) < 16 {
		return fmt.Errorf("authentication failed: daemon sent an invalid challenge")
	}
	clientNonce := NewAuthNonce()
	var hello HelloResult
	params := AuthParams{Proof: AuthProof(token, challenge.Nonce), Proto: ProtoVersion, ClientNonce: clientNonce}
	if err := authCall(enc, dec, 2, "Auth.Hello", params, &hello); err != nil {
		return err
	}
	if !CheckServerAuthProof(token, challenge.Nonce, clientNonce, hello.ServerProof) {
		if hello.ServerProof == "" {
			return errors.New("authentication failed: the daemon did not prove it holds the token (update jerboad to this version)")
		}
		return errors.New("authentication failed: the daemon's proof does not match this token; the endpoint may not be jerboad")
	}
	// A daemon that advertises a proto version must match ours. A zero/absent
	// proto is a daemon that predates negotiation — leave it to the caller.
	if hello.Proto != 0 && hello.Proto != ProtoVersion {
		return fmt.Errorf(
			"protocol version mismatch: daemon speaks v%d, this jerboa speaks v%d — update jerboa to match the daemon",
			hello.Proto, ProtoVersion)
	}
	return nil
}

func authCall(enc *json.Encoder, dec *json.Decoder, id int64, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal auth: %w", err)
	}
	if err := enc.Encode(Request{JSONRPC: "2.0", ID: id, Method: method, Params: json.RawMessage(raw)}); err != nil {
		return fmt.Errorf("send auth: %w", err)
	}
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		return fmt.Errorf("auth response: %w", err)
	}
	if resp.Error != nil {
		return &authError{code: resp.Error.Code, message: resp.Error.Message}
	}
	if len(resp.Result) > 0 {
		_ = json.Unmarshal(resp.Result, out)
	}
	return nil
}

// authRequiredCode is the daemon's answer to a call made before
// authentication, or to a failed proof.
const authRequiredCode = -32001

// authError is the daemon's refusal of a handshake call.
type authError struct {
	code    int
	message string
}

func (e *authError) Error() string {
	if e.code == ErrCodeBusy {
		return e.message
	}
	return "authentication failed: " + e.message
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("api client close: %w", err)
	}
	return nil
}

// Run creates and starts a VM, returning its info.
func (c *Client) Run(ctx context.Context, p RunParams) (VMInfo, error) {
	var info VMInfo
	if err := c.call(ctx, "VM.Run", p, &info); err != nil {
		return VMInfo{}, fmt.Errorf("client run: %w", err)
	}
	return info, nil
}

// Start boots a stopped VM again. The daemon replaces the stopped entry with
// a fresh VM built from the same config, so the returned info carries a new ID.
func (c *Client) Start(ctx context.Context, id string) (VMInfo, error) {
	var info VMInfo
	if err := c.call(ctx, "VM.Start", IDParams{ID: id}, &info); err != nil {
		return VMInfo{}, fmt.Errorf("client start: %w", err)
	}
	return info, nil
}

// Stop sends a graceful stop request. Set force=true for immediate SIGKILL.
func (c *Client) Stop(ctx context.Context, id string, force bool) error {
	if err := c.call(ctx, "VM.Stop", StopParams{ID: id, Force: force}, nil); err != nil {
		return fmt.Errorf("client stop: %w", err)
	}
	return nil
}

// Kill sends an immediate SIGKILL to the VM.
func (c *Client) Kill(ctx context.Context, id string) error {
	if err := c.call(ctx, "VM.Kill", IDParams{ID: id}, nil); err != nil {
		return fmt.Errorf("client kill: %w", err)
	}
	return nil
}

// Signal sends the named signal to the VM process.
func (c *Client) Signal(ctx context.Context, id, sig string) error {
	if err := c.call(ctx, "VM.Signal", SignalParams{ID: id, Signal: sig}, nil); err != nil {
		return fmt.Errorf("client signal: %w", err)
	}
	return nil
}

// Remove removes the VM with the given id.
func (c *Client) Remove(ctx context.Context, id string) error {
	if err := c.call(ctx, "VM.Remove", IDParams{ID: id}, nil); err != nil {
		return fmt.Errorf("client remove: %w", err)
	}
	return nil
}

// List returns all VMs known to the daemon.
func (c *Client) List(ctx context.Context) ([]VMInfo, error) {
	var infos []VMInfo
	if err := c.call(ctx, "VM.List", nil, &infos); err != nil {
		return nil, fmt.Errorf("client list: %w", err)
	}
	return infos, nil
}

// Get returns the VM with the given id.
func (c *Client) Get(ctx context.Context, id string) (VMInfo, error) {
	var info VMInfo
	if err := c.call(ctx, "VM.Get", IDParams{ID: id}, &info); err != nil {
		return VMInfo{}, fmt.Errorf("client get: %w", err)
	}
	return info, nil
}

// Logs returns captured serial console output for the VM.
func (c *Client) Logs(ctx context.Context, id string) (LogsResponse, error) {
	var resp LogsResponse
	if err := c.call(ctx, "VM.Logs", IDParams{ID: id}, &resp); err != nil {
		return LogsResponse{}, fmt.Errorf("client logs: %w", err)
	}
	return resp, nil
}

// Shutdown asks the daemon to exit cleanly.
func (c *Client) Shutdown(ctx context.Context) error {
	if err := c.call(ctx, "Daemon.Shutdown", nil, nil); err != nil {
		return fmt.Errorf("client shutdown: %w", err)
	}
	return nil
}

// DaemonVersion returns the version string reported by the running daemon.
func (c *Client) DaemonVersion(ctx context.Context) (string, error) {
	info, err := c.DaemonInfo(ctx)
	return info.Version, err
}

// DaemonInfo returns the running daemon's version and the hypervisor it
// runs. The hypervisor is empty from a daemon that predates reporting it.
func (c *Client) DaemonInfo(ctx context.Context) (DaemonInfo, error) {
	var resp map[string]string
	if err := c.call(ctx, "Daemon.Version", nil, &resp); err != nil {
		return DaemonInfo{}, fmt.Errorf("client daemon version: %w", err)
	}
	return DaemonInfo{Version: resp["version"], Hypervisor: DaemonHypervisor{
		Name:                 resp["hypervisor"],
		Reason:               resp["hypervisor_reason"],
		FirecrackerAvailable: resp["firecracker_available"] == "true",
	}}, nil
}

// NetworkCreate creates a new network.
func (c *Client) NetworkCreate(ctx context.Context, name, subnet, driver string) (NetworkInfo, error) {
	var info NetworkInfo
	p := NetworkCreateParams{Name: name, Subnet: subnet, Driver: driver}
	if err := c.call(ctx, "Network.Create", p, &info); err != nil {
		return NetworkInfo{}, fmt.Errorf("client network create: %w", err)
	}
	return info, nil
}

// NetworkList returns all networks.
func (c *Client) NetworkList(ctx context.Context) ([]NetworkInfo, error) {
	var infos []NetworkInfo
	if err := c.call(ctx, "Network.List", nil, &infos); err != nil {
		return nil, fmt.Errorf("client network list: %w", err)
	}
	return infos, nil
}

// NetworkGet returns a single network by name.
func (c *Client) NetworkGet(ctx context.Context, name string) (NetworkInfo, error) {
	var info NetworkInfo
	if err := c.call(ctx, "Network.Get", struct {
		Name string `json:"name"`
	}{Name: name}, &info); err != nil {
		return NetworkInfo{}, fmt.Errorf("client network get: %w", err)
	}
	return info, nil
}

// NetworkRemove deletes a network by name.
func (c *Client) NetworkRemove(ctx context.Context, name string) error {
	if err := c.call(ctx, "Network.Remove", struct {
		Name string `json:"name"`
	}{Name: name}, nil); err != nil {
		return fmt.Errorf("client network remove: %w", err)
	}
	return nil
}

// NetworkAllocateIP allocates an IP address from the network's subnet.
func (c *Client) NetworkAllocateIP(ctx context.Context, networkName string) (string, error) {
	var resp map[string]string
	if err := c.call(ctx, "Network.AllocateIP", struct {
		Network string `json:"network"`
	}{Network: networkName}, &resp); err != nil {
		return "", fmt.Errorf("client network allocate ip: %w", err)
	}
	return resp["ip"], nil
}

// NetworkReleaseIP releases an allocated IP address back to the network.
func (c *Client) NetworkReleaseIP(ctx context.Context, networkName, ip string) error {
	if err := c.call(ctx, "Network.ReleaseIP", struct {
		Network string `json:"network"`
		IP      string `json:"ip"`
	}{Network: networkName, IP: ip}, nil); err != nil {
		return fmt.Errorf("client network release ip: %w", err)
	}
	return nil
}

// DNSResolve resolves a VM name to an IP address inside an optional network.
func (c *Client) DNSResolve(ctx context.Context, name, network string) (DNSRecord, error) {
	var rec DNSRecord
	p := DNSResolveParams{Name: name, Network: network}
	if err := c.call(ctx, "DNS.Resolve", p, &rec); err != nil {
		return DNSRecord{}, fmt.Errorf("client dns resolve: %w", err)
	}
	return rec, nil
}

// DNSList lists resolvable VM records, optionally filtered by network.
func (c *Client) DNSList(ctx context.Context, network string) ([]DNSRecord, error) {
	var recs []DNSRecord
	p := struct {
		Network string `json:"network,omitempty"`
	}{Network: network}
	if err := c.call(ctx, "DNS.List", p, &recs); err != nil {
		return nil, fmt.Errorf("client dns list: %w", err)
	}
	return recs, nil
}

// Inspect returns full details for the VM.
func (c *Client) Inspect(ctx context.Context, id string) (VMDetail, error) {
	var detail VMDetail
	if err := c.call(ctx, "VM.Inspect", IDParams{ID: id}, &detail); err != nil {
		return VMDetail{}, fmt.Errorf("client inspect: %w", err)
	}
	return detail, nil
}

// Stats returns runtime resource usage for the VM.
func (c *Client) Stats(ctx context.Context, id string) (VMStatsResponse, error) {
	var stats VMStatsResponse
	if err := c.call(ctx, "VM.Stats", IDParams{ID: id}, &stats); err != nil {
		return VMStatsResponse{}, fmt.Errorf("client stats: %w", err)
	}
	return stats, nil
}

// NodeList returns cluster member information.
func (c *Client) NodeList(ctx context.Context) (NodeListResponse, error) {
	var resp NodeListResponse
	if err := c.call(ctx, "Node.List", nil, &resp); err != nil {
		return NodeListResponse{}, fmt.Errorf("client node list: %w", err)
	}
	return resp, nil
}

// DNSResolveAll resolves all DNS records matching a name (round-robin).
func (c *Client) DNSResolveAll(ctx context.Context, name, network string) ([]DNSRecord, error) {
	var recs []DNSRecord
	p := DNSResolveParams{Name: name, Network: network}
	if err := c.call(ctx, "DNS.ResolveAll", p, &recs); err != nil {
		return nil, fmt.Errorf("client dns resolve all: %w", err)
	}
	return recs, nil
}

// Attach connects to a VM's serial console and streams output to stdout.
// It blocks until the VM stops or the connection is closed.
// This method takes over the connection for raw reading; do not use the
// client for other calls after Attach.
func (c *Client) Attach(ctx context.Context, id string, out io.Writer) (err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken != nil {
		return c.broken
	}
	stop := abortOn(ctx, c.conn)
	defer func() {
		if stop() {
			err = ended(ctx, err)
		}
	}()

	reqID := c.seq.Add(1)
	params, _ := json.Marshal(AttachParams{ID: id, Framed: true})
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
		return resp.Error
	}
	stream := bufio.NewReader(io.MultiReader(c.dec.Buffered(), c.conn))
	// The JSON-RPC response is newline-terminated. Decode leaves that byte
	// buffered; consume it before interpreting the following binary frames.
	delimiter, err := stream.ReadByte()
	if err != nil {
		return fmt.Errorf("decode attach stream delimiter: %w", err)
	}
	if delimiter != '\n' {
		return fmt.Errorf("decode attach stream delimiter: unexpected byte %q", delimiter)
	}
	var protocol struct {
		Framed bool `json:"framed"`
	}
	if len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, &protocol); err != nil {
			return fmt.Errorf("decode attach protocol: %w", err)
		}
	}
	if !protocol.Framed {
		// Older daemons send an unframed console stream without exit status.
		if _, err := io.Copy(out, stream); err != nil {
			return fmt.Errorf("read attach output: %w", err)
		}
		return nil
	}
	if _, err := io.Copy(out, NewFrameReader(stream)); err != nil {
		return fmt.Errorf("read attach output: %w", err)
	}
	var terminal AttachResult
	if err := json.NewDecoder(stream).Decode(&terminal); err != nil {
		return fmt.Errorf("decode attach terminal status: %w", err)
	}
	if !terminal.Done {
		return errors.New("attach ended without terminal status")
	}
	if terminal.Error != "" {
		return errors.New(terminal.Error)
	}
	return nil
}

// ImageBuild sends an Image.Build request and streams the build context to the
// daemon, which assembles the disk image with mkfs on its own filesystem and
// stores it. contextTar is an uncompressed tar archive containing the program
// binary (at p.Program) plus any package or source files.
//
// Build uses a dedicated connection (the daemon consumes the streamed context
// and closes that connection), leaving the client's persistent connection free
// for concurrent calls.
func (c *Client) ImageBuild(ctx context.Context, p BuildParams, contextTar io.Reader) (_ ImageManifestResult, err error) {
	conn, err := dialDaemon(dialArgs(c.endpoint))
	if err != nil {
		return ImageManifestResult{}, fmt.Errorf("dial build connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	// A dedicated connection: closing it is how ctx ends the exchange.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer func() {
		stop()
		err = ended(ctx, err)
	}()

	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	if c.token != "" {
		if err := authenticate(conn, enc, dec, c.token); err != nil {
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
		return ImageManifestResult{}, resp.Error
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
func (c *Client) VolumeSeed(ctx context.Context, p VolumeSeedParams, contextTar io.Reader) (_ VolumeSeedResult, err error) {
	conn, err := dialDaemon(dialArgs(c.endpoint))
	if err != nil {
		return VolumeSeedResult{}, fmt.Errorf("dial seed connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	// A dedicated connection: closing it is how ctx ends the exchange.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer func() {
		stop()
		err = ended(ctx, err)
	}()

	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	if c.token != "" {
		if err := authenticate(conn, enc, dec, c.token); err != nil {
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
		return VolumeSeedResult{}, resp.Error
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
func (c *Client) ImageList(ctx context.Context) ([]ImageManifestResult, error) {
	var out []ImageManifestResult
	if err := c.call(ctx, "Image.List", nil, &out); err != nil {
		return nil, fmt.Errorf("client image list: %w", err)
	}
	return out, nil
}

// ImageGet returns the manifest for a single name:tag (or sha) reference.
func (c *Client) ImageGet(ctx context.Context, ref string) (ImageManifestResult, error) {
	var out ImageManifestResult
	p := struct {
		Ref string `json:"ref"`
	}{Ref: ref}
	if err := c.call(ctx, "Image.Get", p, &out); err != nil {
		return ImageManifestResult{}, fmt.Errorf("client image get: %w", err)
	}
	return out, nil
}

// ImageTag gives a stored image another name:tag.
func (c *Client) ImageTag(ctx context.Context, p ImageTagParams) (ImageManifestResult, error) {
	var out ImageManifestResult
	if err := c.call(ctx, "Image.Tag", p, &out); err != nil {
		return out, fmt.Errorf("client image tag: %w", err)
	}
	return out, nil
}

// ImagePush pushes a stored image to its registry.
func (c *Client) ImagePush(ctx context.Context, p ImagePushParams) (ImagePushResult, error) {
	var out ImagePushResult
	if err := c.call(ctx, "Image.Push", p, &out); err != nil {
		return out, fmt.Errorf("client image push: %w", err)
	}
	return out, nil
}

// ImagePull pulls an image from a registry into the daemon's store.
func (c *Client) ImagePull(ctx context.Context, p ImagePullParams) (ImagePullResult, error) {
	var out ImagePullResult
	if err := c.call(ctx, "Image.Pull", p, &out); err != nil {
		return out, fmt.Errorf("client image pull: %w", err)
	}
	return out, nil
}

// ImageRecipe returns the recipe document an image tag was built from.
func (c *Client) ImageRecipe(ctx context.Context, ref string) (json.RawMessage, error) {
	var out json.RawMessage
	p := struct {
		Ref string `json:"ref"`
	}{Ref: ref}
	if err := c.call(ctx, "Image.Recipe", p, &out); err != nil {
		return nil, fmt.Errorf("client image recipe: %w", err)
	}
	return out, nil
}

// ImageSBOM returns the software bill of materials stored with an image.
func (c *Client) ImageSBOM(ctx context.Context, ref string) (json.RawMessage, error) {
	var out json.RawMessage
	p := struct {
		Ref string `json:"ref"`
	}{Ref: ref}
	if err := c.call(ctx, "Image.SBOM", p, &out); err != nil {
		return nil, fmt.Errorf("client image sbom: %w", err)
	}
	return out, nil
}

// ImageRemove deletes a name:tag (or sha) reference from the daemon's store.
func (c *Client) ImageRemove(ctx context.Context, ref string) error {
	p := struct {
		Ref string `json:"ref"`
	}{Ref: ref}
	if err := c.call(ctx, "Image.Remove", p, nil); err != nil {
		return fmt.Errorf("client image remove: %w", err)
	}
	return nil
}

// SnapshotCreate pauses VM id, captures snapshot name and resumes the VM.
func (c *Client) SnapshotCreate(ctx context.Context, id, name string) (SnapshotInfo, error) {
	var info SnapshotInfo
	if err := c.call(ctx, "VM.SnapshotCreate", SnapshotParams{ID: id, Name: name}, &info); err != nil {
		return SnapshotInfo{}, fmt.Errorf("client snapshot create: %w", err)
	}
	return info, nil
}

// SnapshotRestore restores stopped VM id in place from snapshot name.
func (c *Client) SnapshotRestore(ctx context.Context, id, name string) (VMInfo, error) {
	var info VMInfo
	if err := c.call(ctx, "VM.SnapshotRestore", SnapshotParams{ID: id, Name: name}, &info); err != nil {
		return VMInfo{}, fmt.Errorf("client snapshot restore: %w", err)
	}
	return info, nil
}

// SnapshotList returns every snapshot store entry.
func (c *Client) SnapshotList(ctx context.Context) ([]SnapshotInfo, error) {
	var out []SnapshotInfo
	if err := c.call(ctx, "Snapshot.List", struct{}{}, &out); err != nil {
		return nil, fmt.Errorf("client snapshot list: %w", err)
	}
	return out, nil
}

// SnapshotInspect returns one snapshot's metadata.
func (c *Client) SnapshotInspect(ctx context.Context, name string) (SnapshotInfo, error) {
	var info SnapshotInfo
	if err := c.call(ctx, "Snapshot.Inspect", SnapshotParams{Name: name}, &info); err != nil {
		return SnapshotInfo{}, fmt.Errorf("client snapshot inspect: %w", err)
	}
	return info, nil
}

// SnapshotRemove deletes a snapshot that no restore is using.
func (c *Client) SnapshotRemove(ctx context.Context, name string) error {
	if err := c.call(ctx, "Snapshot.Remove", SnapshotParams{Name: name}, nil); err != nil {
		return fmt.Errorf("client snapshot remove: %w", err)
	}
	return nil
}

// call sends one request and waits for its answer. It returns as soon as ctx
// ends; the connection cannot be used again after that, because the daemon's
// answer to the abandoned request is still on its way.
func (c *Client) call(ctx context.Context, method string, params any, out any) (err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken != nil {
		return c.broken
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	// cut records that the exchange itself failed, as opposed to the daemon
	// answering with an error.
	cut := false
	stop := abortOn(ctx, c.conn)
	defer func() {
		if !stop() {
			return
		}
		if !cut {
			// The answer arrived as ctx ended: the connection is intact.
			_ = c.conn.SetDeadline(time.Time{})
			return
		}
		c.broken = fmt.Errorf("the connection was abandoned when %s was interrupted: %w", method, ctx.Err())
		err = ended(ctx, err)
	}()

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
		cut = true
		return fmt.Errorf("encode request: %w", err)
	}
	var resp Response
	if err := c.dec.Decode(&resp); err != nil {
		cut = true
		return fmt.Errorf("decode response: %w", err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out != nil && resp.Result != nil {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("unmarshal result: %w", err)
		}
	}
	return nil
}

func (c *Client) VolumeRemove(ctx context.Context, name, disk string) error {
	return c.call(ctx, "Volume.Remove", VolumeRemoveParams{Name: name, DiskPath: disk}, nil)
}

// SystemDF reports the disk space the daemon's images, VMs, volumes,
// snapshots and caches take.
func (c *Client) SystemDF(ctx context.Context) (SystemDF, error) {
	var out SystemDF
	if err := c.call(ctx, "System.DF", nil, &out); err != nil {
		return SystemDF{}, err
	}
	return out, nil
}

// SystemPrune removes what nothing refers to any more, and whatever else p
// selects.
func (c *Client) SystemPrune(ctx context.Context, p SystemPruneParams) (SystemPruneResult, error) {
	var out SystemPruneResult
	if err := c.call(ctx, "System.Prune", p, &out); err != nil {
		return SystemPruneResult{}, err
	}
	return out, nil
}

// SystemCheck has the daemon examine its own state.
func (c *Client) SystemCheck(ctx context.Context, p SystemCheckParams) (SystemCheckResult, error) {
	var out SystemCheckResult
	if err := c.call(ctx, "System.Check", p, &out); err != nil {
		return SystemCheckResult{}, err
	}
	return out, nil
}
