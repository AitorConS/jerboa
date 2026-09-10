//go:build linux || (darwin && arm64)

package apiserver

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/image"
	"github.com/AitorConS/jerboa/internal/metrics"
	"github.com/AitorConS/jerboa/internal/network"
	"github.com/AitorConS/jerboa/internal/scheduler"
	"github.com/AitorConS/jerboa/internal/vm"
	"github.com/AitorConS/jerboa/internal/volume"
)

type ClusterMemberLister interface {
	Members() []ClusterMember
}

type ClusterMember struct {
	ID       string
	Addr     string
	Status   string
	VMCount  int
	CPUCap   int
	MemCap   int64
	LastSeen time.Time
}

// Server listens on a Unix socket and dispatches JSON-RPC requests to a
// vm.Manager.
type Server struct {
	mgr          vm.Manager
	netStore     *network.Store
	listener     net.Listener
	shutdownFn   func()
	version      string
	resolver     *scheduler.Resolver
	cluster      ClusterMemberLister
	imgStore     *image.Store
	volStore     *volume.Store
	mkfsMu       sync.Mutex
	mkfsResolver func(context.Context) (image.MkfsFunc, error)
	mkfsCached   image.MkfsFunc
	volFmtMu     sync.Mutex
	volFmtResolv func(context.Context) (volume.Formatter, error)
	volFmtCached volume.Formatter
	volSeedMu    sync.Mutex
	volSeedResol func(context.Context) (volume.Seeder, error)
	volSeedCache volume.Seeder
	authToken    string
	collectors   *metrics.Collectors
	startingMu   sync.Mutex
	starting     map[string]struct{}
	// nameMu serializes the VM-name uniqueness check with VM creation so two
	// concurrent runs cannot both register the same name.
	nameMu     sync.Mutex
	resourceMu sync.Mutex
}

// createUnique registers a VM, rejecting a name already held by another VM
// (Docker-like semantics). A duplicate name makes name-based addressing
// ambiguous and breaks DNS resolution of that name entirely, so it must be
// refused at run time. The check and the create are done under nameMu so
// concurrent same-name runs cannot race past it. Internal restart/replace paths
// create replacements directly on the store and intentionally bypass this, so a
// legitimate same-name replacement of a stopped VM still works.
func (s *Server) createUnique(ctx context.Context, cfg vm.Config) (*vm.VM, error) {
	if cfg.Name == "" {
		return s.mgr.Create(ctx, cfg)
	}
	s.nameMu.Lock()
	defer s.nameMu.Unlock()
	for _, existing := range s.mgr.List() {
		if existing.Cfg.Name == cfg.Name {
			return nil, fmt.Errorf("vm name %q is already in use", cfg.Name)
		}
	}
	return s.mgr.Create(ctx, cfg)
}

// SetCollectors attaches Prometheus collectors so VM start/stop/kill RPCs
// increment lifecycle counters. Optional: nil-safe when metrics are disabled.
func (s *Server) SetCollectors(c *metrics.Collectors) {
	s.collectors = c
}

// SetAuthToken requires every connection to authenticate via an Auth.Hello
// handshake carrying token before any other method is dispatched. An empty
// token (the default) disables authentication. Call once before Serve.
func (s *Server) SetAuthToken(token string) {
	s.authToken = token
}

// SetVolumeStore attaches the daemon's volume store, enabling Volume.Seed to
// resolve and validate store-owned volume disk paths server-side.
func (s *Server) SetVolumeStore(store *volume.Store) {
	s.volStore = store
}

// NewServer creates a Server that will listen on endpoint. The endpoint may
// carry a scheme (unix:// or tcp://); a bare value is treated as a Unix socket
// path. For unix endpoints any stale socket file is removed before binding.
// shutdownFn is called (in a goroutine) when a Daemon.Shutdown RPC is received;
// pass nil to disable remote shutdown.
// version is returned by Daemon.Version RPC; pass "" if unknown.
func NewServer(mgr vm.Manager, netStore *network.Store, endpoint string, shutdownFn func(), version string, clusterLister ClusterMemberLister) (*Server, error) {
	l, err := api.Listen(endpoint)
	if err != nil {
		return nil, err
	}
	return &Server{mgr: mgr, netStore: netStore, listener: l, shutdownFn: shutdownFn, version: version, resolver: scheduler.NewResolver(mgr), cluster: clusterLister}, nil
}

// Serve accepts connections and handles them until ctx is canceled.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		if err := s.listener.Close(); err != nil {
			slog.Warn("api server close listener", "err", err)
		}
	}()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil //nolint:nilerr // ctx cancellation is graceful shutdown — not an error
			}
			return fmt.Errorf("api server accept: %w", err)
		}
		go s.handle(ctx, conn)
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	// currentReqID is the ID of the request being processed; the panic handler
	// uses it to address the error response. Safe without synchronization: the
	// loop below and the deferred recover run in the same goroutine.
	var currentReqID int64
	// A panic in any handler must never crash the daemon: recover here, log the
	// stack, and try to return an error to the client instead of resetting the
	// connection (which surfaces to the CLI as "decode response: EOF"). The
	// deferred conn.Close still runs.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("api server: recovered panic in handler", "panic", r, "stack", string(debug.Stack()))
			_ = enc.Encode(api.Response{JSONRPC: "2.0", ID: currentReqID, Error: &api.RPCError{Code: -32603, Message: fmt.Sprintf("internal error: %v", r)}})
		}
		if err := conn.Close(); err != nil {
			slog.Warn("api server close conn", "err", err)
		}
	}()
	authed := s.authToken == ""
	for dec.More() {
		var req api.Request
		if err := dec.Decode(&req); err != nil {
			return
		}
		currentReqID = req.ID
		if !authed {
			// Until authenticated, only Auth.Hello is accepted. Any failure
			// closes the connection.
			if req.Method != "Auth.Hello" || !s.checkAuth(req.Params) {
				_ = enc.Encode(api.Response{JSONRPC: "2.0", ID: req.ID, Error: &api.RPCError{Code: -32001, Message: "authentication required"}})
				return
			}
			// Refuse a client whose wire protocol differs from ours. A client
			// that omits proto (0) predates negotiation and is left alone.
			if proto := helloProto(req.Params); proto != 0 && proto != api.ProtoVersion {
				_ = enc.Encode(api.Response{JSONRPC: "2.0", ID: req.ID, Error: &api.RPCError{Code: -32002, Message: fmt.Sprintf(
					"protocol version mismatch: daemon speaks v%d, client speaks v%d — update jerboa to match the daemon",
					api.ProtoVersion, proto)}})
				return
			}
			authed = true
			_ = enc.Encode(api.Response{JSONRPC: "2.0", ID: req.ID, Result: helloResult()})
			continue
		}
		result, rpcErr := s.dispatch(ctx, &req, conn, dec)
		if result == attachHandled {
			return
		}
		resp := api.Response{JSONRPC: "2.0", ID: req.ID}
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			raw, err := json.Marshal(result)
			if err != nil {
				slog.Warn("api server marshal result", "err", err)
				return
			}
			resp.Result = raw
		}
		if err := enc.Encode(resp); err != nil {
			slog.Warn("api server encode response", "err", err)
			return
		}
	}
}

// checkAuth reports whether params carries the configured auth token, using a
// constant-time comparison.
func (s *Server) checkAuth(params json.RawMessage) bool {
	var p api.AuthParams
	if err := json.Unmarshal(params, &p); err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(p.Token), []byte(s.authToken)) == 1
}

// helloProto extracts the client's advertised wire protocol version from the
// Auth.Hello params, returning 0 when absent or unparseable.
func helloProto(params json.RawMessage) int {
	var p api.AuthParams
	if err := json.Unmarshal(params, &p); err != nil {
		return 0
	}
	return p.Proto
}

// helloResult is the Auth.Hello acknowledgement, carrying the daemon's wire
// protocol version so the client can detect an incompatible (older) daemon.
func helloResult() json.RawMessage {
	raw, _ := json.Marshal(api.HelloResult{Status: "ok", Proto: api.ProtoVersion})
	return raw
}

// attachHandled is a sentinel value returned by dispatch when VM.Attach
// has taken over the connection and no response should be sent.
var attachHandled = struct{}{}

func (s *Server) dispatch(ctx context.Context, req *api.Request, conn net.Conn, dec *json.Decoder) (any, *api.RPCError) {
	switch req.Method {
	case "Image.Build":
		// Build streams its context after the request. Read the decoder's
		// leftover buffer first, then the raw connection. json.Encoder writes a
		// trailing newline after the request that the decoder leaves buffered,
		// so skip any leading whitespace before the binary frame stream.
		br := bufio.NewReader(io.MultiReader(dec.Buffered(), conn))
		skipLeadingWhitespace(br)
		s.handleBuild(ctx, req.Params, api.NewFrameReader(br), conn, req.ID)
		return attachHandled, nil
	case "Volume.Seed":
		// Like Image.Build, Volume.Seed streams its context tar after the request.
		br := bufio.NewReader(io.MultiReader(dec.Buffered(), conn))
		skipLeadingWhitespace(br)
		s.handleVolumeSeed(ctx, req.Params, api.NewFrameReader(br), conn, req.ID)
		return attachHandled, nil
	case "Image.List":
		return s.handleImageList()
	case "Image.Get":
		return s.handleImageGet(req.Params)
	case "Image.Remove":
		return s.handleImageRemove(req.Params)
	case "VM.Run":
		return s.handleRun(ctx, req.Params)
	case "VM.Start":
		return s.handleStart(ctx, req.Params)
	case "VM.Stop":
		return s.handleStop(ctx, req.Params)
	case "VM.Kill":
		return s.handleKill(ctx, req.Params)
	case "VM.Signal":
		return s.handleSignal(ctx, req.Params)
	case "VM.Remove":
		return s.handleRemove(ctx, req.Params)
	case "VM.List":
		return s.handleList(ctx)
	case "VM.Get":
		return s.handleGet(req.Params)
	case "VM.Logs":
		return s.handleLogs(req.Params)
	case "VM.Inspect":
		return s.handleInspect(req.Params)
	case "VM.Attach":
		s.handleAttach(ctx, req.Params, conn, req.ID)
		return attachHandled, nil
	case "Daemon.Shutdown":
		return s.handleDaemonShutdown()
	case "Daemon.Version":
		return s.handleDaemonVersion()
	case "Network.Create":
		return s.handleNetworkCreate(req.Params)
	case "Network.List":
		return s.handleNetworkList()
	case "Network.Get":
		return s.handleNetworkGet(req.Params)
	case "Network.Remove":
		return s.handleNetworkRemove(req.Params)
	case "Volume.Remove":
		return s.handleVolumeRemove(req.Params)
	case "Network.AllocateIP":
		return s.handleNetworkAllocateIP(req.Params)
	case "Network.ReleaseIP":
		return s.handleNetworkReleaseIP(req.Params)
	case "VM.Stats":
		return s.handleStats(req.Params)
	case "DNS.Resolve":
		return s.handleDNSResolve(req.Params)
	case "DNS.List":
		return s.handleDNSList(req.Params)
	case "Node.List":
		return s.handleNodeList()
	case "DNS.ResolveAll":
		return s.handleDNSResolveAll(req.Params)
	default:
		return nil, &api.RPCError{Code: -32601, Message: "method not found: " + req.Method}
	}
}

// resolveImageRef turns an image reference into a bootable disk path. A value
// that looks like a filesystem path is returned unchanged; otherwise it is
// resolved against the daemon image store.
func (s *Server) resolveImageRef(image string) (string, error) {
	if image == "" {
		return "", fmt.Errorf("image is required")
	}
	if looksLikePath(image) {
		return image, nil
	}
	if s.imgStore == nil {
		return "", fmt.Errorf("image store disabled: cannot resolve image reference %q", image)
	}
	_, diskPath, err := s.imgStore.Get(image)
	if err != nil {
		// The store error nests its own "image store get <ref>: <ref> not found"
		// which is internal plumbing; users only need the clean statement.
		return "", fmt.Errorf("image %q not found", image)
	}
	return diskPath, nil
}

// looksLikePath reports whether s is a filesystem path rather than a name:tag
// image reference. name:tag values contain a ':' but no path separators and no
// Windows drive prefix.
func looksLikePath(s string) bool {
	return strings.ContainsAny(s, "/\\") || (len(s) >= 3 && s[1] == ':' && (s[2] == '/' || s[2] == '\\'))
}

// maskFromCIDR returns the prefix length of a CIDR subnet ("10.100.0.0/24" →
// "24"), or "" when the subnet carries no prefix.
func maskFromCIDR(subnet string) string {
	if i := strings.LastIndex(subnet, "/"); i >= 0 {
		return subnet[i+1:]
	}
	return ""
}

func (s *Server) handleRun(ctx context.Context, params json.RawMessage) (any, *api.RPCError) {
	var p api.RunParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	imagePath := p.ImagePath
	imageDigest := ""
	var resolvedManifest *image.Manifest
	if p.Image != "" {
		if s.imgStore != nil && !looksLikePath(p.Image) {
			m, disk, err := s.imgStore.Resolve(p.Image)
			if err != nil {
				return nil, &api.RPCError{Code: -32000, Message: err.Error()}
			}
			resolvedManifest = &m
			imagePath = disk
			imageDigest = m.DiskDigest
			p.Image = imageDigest
		} else {
			resolved, err := s.resolveImageRef(p.Image)
			if err != nil {
				return nil, &api.RPCError{Code: -32000, Message: err.Error()}
			}
			imagePath = resolved
		}
	}
	if rerr := s.ensureVolumesFormatted(ctx, p.Volumes); rerr != nil {
		return nil, rerr
	}

	architecture := ""
	if runtime.GOOS == "darwin" && (p.ImagePath != "" || looksLikePath(p.Image)) {
		architecture = "arm64"
		if p.EmulateX86 {
			architecture = "amd64"
		}
	}
	if m := resolvedManifest; m != nil {
		architecture = m.Architecture
		if runtime.GOOS == "darwin" && m.Architecture != "arm64" && !p.EmulateX86 {
			return nil, &api.RPCError{Code: -32000, Message: "native macOS requires an ARM64 image; rebuild with --platform linux/arm64 or explicitly use --emulate-x86"}
		}
	}

	// The daemon owns each reservation until creation succeeds, rolling it back on failure.
	assignedIP := ""
	if p.NetworkName != "" {
		n, nerr := s.netStore.Get(p.NetworkName)
		if nerr != nil {
			// Unknown (ad-hoc) network. It can only work if the client fully
			// specified the addressing itself; without an IP the guest configures
			// no address and the tap is never usable, so fail with the store's
			// "not found" rather than booting a VM that can never reach the network.
			if p.IPAddress == "" {
				return nil, &api.RPCError{Code: -32000, Message: nerr.Error()}
			}
		} else {
			if p.IPAddress == "" {
				ip, aerr := s.netStore.AllocateIP(p.NetworkName)
				if aerr != nil {
					return nil, &api.RPCError{Code: -32000, Message: "allocate ip: " + aerr.Error()}
				}
				p.IPAddress = ip.String()
				assignedIP = p.IPAddress
			} else if rerr := s.netStore.ReserveIP(p.NetworkName, p.IPAddress); rerr != nil {
				return nil, &api.RPCError{Code: -32000, Message: "reserve ip: " + rerr.Error()}
			} else {
				assignedIP = p.IPAddress
			}
			if p.GatewayIP == "" {
				p.GatewayIP = n.Gateway
			}
			if p.BridgeName == "" {
				p.BridgeName = n.Bridge
			}
			if p.SubnetMask == "" {
				p.SubnetMask = maskFromCIDR(n.Subnet)
			}
		}
	}

	// Inherit defaults baked into the image manifest ([run] in unikernel.toml)
	// for any run parameter the client left unset. Explicit run flags always win.
	memory, cpus := p.Memory, p.CPUs
	portMaps := portMapsFromSpec(p.PortMaps)
	if m := resolvedManifest; m != nil {
		if memory == "" {
			memory = m.Config.Memory
		}
		if cpus == 0 {
			cpus = m.Config.CPUs
		}
		// Baked ports only publish when the VM joins a network; without one
		// there is nothing to forward through, so leave them inert.
		if len(portMaps) == 0 && (p.NetworkName != "" || runtime.GOOS == "darwin") && len(m.Config.Ports) > 0 {
			if specs, perr := api.ParsePortMaps(m.Config.Ports); perr == nil {
				portMaps = portMapsFromSpec(specs)
			}
		}
	}
	// Final fallback when neither a flag nor a manifest default supplied a value
	// (e.g. file-path runs, which have no manifest). The VM manager requires a
	// non-empty memory and a positive CPU count.
	if memory == "" {
		memory = "256M"
	}
	if cpus == 0 {
		cpus = 1
	}

	cfg := vm.Config{
		EmulateX86:   p.EmulateX86,
		Architecture: architecture,
		ImagePath:    imagePath,
		ImageDigest:  imageDigest,
		ImageRef:     p.Image,
		Memory:       memory,
		CPUs:         cpus,
		NetworkName:  p.NetworkName,
		PortMaps:     portMaps,
		Env:          p.Env,
		Name:         p.Name,
		Volumes:      volumeMountsFromSpec(p.Volumes),
		Attach:       p.Attach,
		IPAddress:    p.IPAddress,
		GatewayIP:    p.GatewayIP,
		BridgeName:   p.BridgeName,
		SubnetMask:   p.SubnetMask,
		CPUShares:    p.CPUShares,
		MemoryMax:    p.MemoryMax,
		DiskIOPS:     p.DiskIOPS,
		DiskBPS:      p.DiskBPS,
	}
	if p.HealthCheck != nil {
		cfg.HealthCheck = &vm.HealthCheckConfig{
			Type:     p.HealthCheck.Type,
			Port:     p.HealthCheck.Port,
			Path:     p.HealthCheck.Path,
			Interval: time.Duration(p.HealthCheck.Interval) * time.Second,
			Timeout:  time.Duration(p.HealthCheck.Timeout) * time.Second,
			Retries:  p.HealthCheck.Retries,
		}
	}
	if p.Restart != nil {
		cfg.Restart = vm.RestartConfig{
			Policy:     vm.RestartPolicy(p.Restart.Policy),
			MaxRetries: p.Restart.MaxRetries,
		}
	}
	v, err := s.createUnique(ctx, cfg)
	if err != nil {
		// The VM was never registered, so nothing references any IP the daemon
		// assigned above (allocated or reserved); return it to the pool.
		if assignedIP != "" {
			if relErr := s.netStore.ReleaseIP(p.NetworkName, assignedIP); relErr != nil {
				slog.Debug("run: release ip after create failure", "network", p.NetworkName, "ip", assignedIP, "err", relErr)
			}
		}
		s.recordVMError()
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	if err := s.mgr.Start(ctx, v.ID); err != nil {
		if removeErr := s.mgr.Remove(ctx, v.ID); removeErr != nil {
			return nil, &api.RPCError{Code: -32000, Message: fmt.Sprintf("%v; cleanup VM %s: %v", err, v.ID, removeErr)}
		}
		if assignedIP != "" {
			if releaseErr := s.netStore.ReleaseIP(p.NetworkName, assignedIP); releaseErr != nil {
				return nil, &api.RPCError{Code: -32000, Message: fmt.Sprintf("%v; release IP: %v", err, releaseErr)}
			}
		}
		s.recordVMError()
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	if s.collectors != nil {
		s.collectors.VMStartsTotal.Inc()
	}
	if p.AutoRemove {
		go s.autoRemove(ctx, v)
	}
	return toInfo(v), nil
}

func (s *Server) autoRemove(ctx context.Context, v *vm.VM) {
	<-v.Done()
	if err := s.mgr.Remove(ctx, v.ID); err != nil {
		slog.Debug("auto-remove vm", "vm_id", v.ID, "err", err)
	}
}

// recordVMError increments the VM error counter, if metrics are enabled.
func (s *Server) recordVMError() {
	if s.collectors != nil {
		s.collectors.VMErrorsTotal.Inc()
	}
}

// claimStart marks a VM as having a start in flight. It returns false when
// another VM.Start already holds the claim, so concurrent starts for the same
// VM cannot both pass the stopped-state check and create duplicate
// replacements. Callers must releaseStart the same id when done.
func (s *Server) claimStart(id string) bool {
	s.startingMu.Lock()
	defer s.startingMu.Unlock()
	if _, busy := s.starting[id]; busy {
		return false
	}
	if s.starting == nil {
		s.starting = make(map[string]struct{})
	}
	s.starting[id] = struct{}{}
	return true
}

func (s *Server) releaseStart(id string) {
	s.startingMu.Lock()
	delete(s.starting, id)
	s.startingMu.Unlock()
}

// handleStart boots a stopped VM again. The original hypervisor process is
// gone, so — like the restart-policy path in the VM monitor — it creates a
// replacement VM with the same config, starts it, and removes the stopped
// registry entry. The response carries the replacement's (new) ID. A VM still
// in "created" (registered but never started) is started in place.
func (s *Server) handleStart(ctx context.Context, params json.RawMessage) (any, *api.RPCError) {
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	var p api.IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	old, err := s.mgr.Get(p.ID)
	if err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	// Serialize on the resolved ID (p.ID may be a name or prefix) across the
	// whole check→create→start→remove sequence; a second concurrent start
	// would otherwise also see StateStopped and boot a duplicate replacement
	// sharing the VM's static IP.
	if !s.claimStart(old.ID) {
		return nil, &api.RPCError{Code: -32000, Message: fmt.Sprintf("vm %s: start already in progress", old.ID)}
	}
	defer s.releaseStart(old.ID)
	// Re-resolve under the claim: a start that completed between the lookup
	// above and the claim has replaced and removed this VM.
	old, err = s.mgr.Get(old.ID)
	if err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	switch st := old.GetState(); st {
	case vm.StateCreated:
		if err := s.mgr.Start(ctx, old.ID); err != nil {
			s.recordVMError()
			return nil, &api.RPCError{Code: -32000, Message: err.Error()}
		}
		if s.collectors != nil {
			s.collectors.VMStartsTotal.Inc()
		}
		return toInfo(old), nil
	case vm.StateStopped:
		// fall through to the replace-and-start path below
	default:
		return nil, &api.RPCError{Code: -32000, Message: fmt.Sprintf("vm %s is %s; only stopped VMs can be started", old.ID, st)}
	}
	v, err := s.mgr.Create(ctx, old.Cfg)
	if err != nil {
		s.recordVMError()
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	if err := s.mgr.Start(ctx, v.ID); err != nil {
		s.recordVMError()
		if rerr := s.mgr.Remove(ctx, v.ID); rerr != nil {
			slog.Warn("vm start: failed to remove unstartable replacement", "vm_id", v.ID, "err", rerr)
		}
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	if err := s.mgr.Remove(ctx, old.ID); err != nil {
		slog.Warn("vm start: failed to remove stopped predecessor", "vm_id", old.ID, "err", err)
	}
	if s.collectors != nil {
		s.collectors.VMStartsTotal.Inc()
	}
	return toInfo(v), nil
}

func (s *Server) handleStop(ctx context.Context, params json.RawMessage) (any, *api.RPCError) {
	var p api.StopParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	var err error
	if p.Force {
		err = s.mgr.Kill(ctx, p.ID)
	} else {
		err = s.mgr.Stop(ctx, p.ID)
	}
	if err != nil {
		s.recordVMError()
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	if s.collectors != nil {
		s.collectors.VMStopsTotal.Inc()
	}
	return map[string]string{"status": "ok"}, nil
}

func (s *Server) handleKill(ctx context.Context, params json.RawMessage) (any, *api.RPCError) {
	var p api.IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if err := s.mgr.Kill(ctx, p.ID); err != nil {
		s.recordVMError()
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	if s.collectors != nil {
		s.collectors.VMStopsTotal.Inc()
	}
	return map[string]string{"status": "ok"}, nil
}

func (s *Server) handleSignal(ctx context.Context, params json.RawMessage) (any, *api.RPCError) {
	var p api.SignalParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	sig, err := parseSig(p.Signal)
	if err != nil {
		return nil, &api.RPCError{Code: -32602, Message: err.Error()}
	}
	if err := s.mgr.Signal(ctx, p.ID, sig); err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	return map[string]string{"status": "ok"}, nil
}

func (s *Server) handleRemove(ctx context.Context, params json.RawMessage) (any, *api.RPCError) {
	var p api.IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if err := s.mgr.Remove(ctx, p.ID); err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	return map[string]string{"status": "ok"}, nil
}

func (s *Server) handleList(_ context.Context) (any, *api.RPCError) {
	vms := s.mgr.List()
	infos := make([]api.VMInfo, len(vms))
	for i, v := range vms {
		infos[i] = toInfo(v)
	}
	return infos, nil
}

func (s *Server) handleGet(params json.RawMessage) (any, *api.RPCError) {
	var p api.IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	v, err := s.mgr.Get(p.ID)
	if err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	return toInfo(v), nil
}

func (s *Server) handleLogs(params json.RawMessage) (any, *api.RPCError) {
	var p api.IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	v, err := s.mgr.Get(p.ID)
	if err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	return api.LogsResponse{ID: v.ID, Logs: string(v.Logs())}, nil
}

func (s *Server) handleDaemonShutdown() (any, *api.RPCError) {
	if s.shutdownFn != nil {
		go s.shutdownFn()
	}
	return map[string]string{"status": "ok"}, nil
}

func (s *Server) handleDaemonVersion() (any, *api.RPCError) {
	return map[string]string{"version": s.version}, nil
}

func (s *Server) handleInspect(params json.RawMessage) (any, *api.RPCError) {
	var p api.IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	v, err := s.mgr.Get(p.ID)
	if err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	return toDetail(v), nil
}

func (s *Server) handleStats(params json.RawMessage) (any, *api.RPCError) {
	var p api.IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	v, err := s.mgr.Get(p.ID)
	if err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	stats := v.Stats()
	return api.VMStatsResponse{
		ID:         stats.ID,
		State:      stats.State,
		CPUPct:     stats.CPUPct,
		MemBytes:   stats.MemBytes,
		DiskBytes:  stats.DiskBytes,
		NetRxBytes: stats.NetRxBytes,
		NetTxBytes: stats.NetTxBytes,
		Timestamp:  stats.Timestamp.Format(time.RFC3339),
		Source:     stats.Source,
	}, nil
}

// imageDisplay returns the image reference the VM was created from (e.g.
// "flaskapp:latest") for display, falling back to the raw disk path when the
// VM was started without a registered image reference.
func imageDisplay(cfg vm.Config) string {
	if cfg.ImageRef != "" {
		return cfg.ImageRef
	}
	return cfg.ImagePath
}

func toInfo(v *vm.VM) api.VMInfo {
	return api.VMInfo{
		ID:     v.ID,
		State:  string(v.GetState()),
		Image:  imageDisplay(v.Cfg),
		Name:   v.Cfg.Name,
		Health: string(v.GetHealthStatus()),
	}
}

func toDetail(v *vm.VM) api.VMDetail {
	d := api.VMDetail{
		ID:              v.ID,
		State:           string(v.GetState()),
		Image:           imageDisplay(v.Cfg),
		Name:            v.Cfg.Name,
		Memory:          v.Cfg.Memory,
		CPUs:            v.Cfg.CPUs,
		Ports:           portMapsToSpec(v.Cfg.PortMaps),
		Env:             v.Cfg.Env,
		Volumes:         volumeMountsToSpec(v.Cfg.Volumes),
		IPAddress:       v.Cfg.IPAddress,
		GatewayIP:       v.Cfg.GatewayIP,
		CreatedAt:       v.CreatedAt.Format(time.RFC3339),
		DaemonRecovered: v.DaemonRecovered,
		Health:          string(v.GetHealthStatus()),
		RestartCount:    v.GetRestartCount(),
		RestartPolicy:   string(v.Cfg.Restart.Policy),
		DiskIOPS:        v.Cfg.DiskIOPS,
		DiskBPS:         v.Cfg.DiskBPS,
		Warnings:        v.Warnings(),
	}
	startedAt, stoppedAt := v.GetTimes()
	if startedAt != nil {
		s := startedAt.Format(time.RFC3339)
		d.StartedAt = &s
	}
	if stoppedAt != nil {
		s := stoppedAt.Format(time.RFC3339)
		d.StoppedAt = &s
	}
	return d
}

// portMapsFromSpec converts API wire types to vm domain types.
func portMapsFromSpec(specs []api.PortMapSpec) []vm.PortMap {
	out := make([]vm.PortMap, len(specs))
	for i, s := range specs {
		out[i] = vm.PortMap{
			HostPort:  s.HostPort,
			GuestPort: s.GuestPort,
			Protocol:  vm.PortProtocol(s.Protocol),
			BindAddr:  s.BindAddr,
		}
	}
	return out
}

// ensureVolumesFormatted formats each attached volume as a labeled TFS
// filesystem before the VM starts, unless it is already formatted (idempotent,
// so existing data is preserved). Read-only volumes are never formatted — they
// must already hold a filesystem. Returns an RPC error on failure.
func (s *Server) ensureVolumesFormatted(ctx context.Context, specs []api.VolumeMountSpec) *api.RPCError {
	if len(specs) == 0 {
		return nil
	}
	var formatter volume.Formatter
	for _, sp := range specs {
		if sp.DiskPath == "" || sp.GuestPath == "" {
			continue // bare block device with no mount point; nothing to format
		}
		if sp.ReadOnly {
			// Read-only volumes are never formatted at attach time (they must
			// already hold a filesystem). A freshly created, still-raw volume
			// mounted read-only therefore fails deep in the guest with a cryptic
			// "tfs magic mismatch". Detect the empty/unformatted case here and
			// fail the run with an actionable message instead.
			formatted, err := volume.IsFormatted(sp.DiskPath)
			if err != nil {
				return &api.RPCError{Code: -32000, Message: "volume probe: " + err.Error()}
			}
			if !formatted {
				return &api.RPCError{Code: -32000, Message: fmt.Sprintf(
					"read-only volume mounted at %s is empty (no filesystem): a read-only volume is never formatted at attach time, so seed it first (e.g. jerboa volume create --seed-pkg / jerboa volume seed) before mounting it :ro",
					sp.GuestPath)}
			}
			continue
		}
		formatted, err := volume.IsFormatted(sp.DiskPath)
		if err != nil {
			return &api.RPCError{Code: -32000, Message: "volume probe: " + err.Error()}
		}
		if formatted {
			continue
		}
		if formatter == nil {
			f, err := s.resolveVolumeFormatter(ctx)
			if err != nil {
				return &api.RPCError{Code: -32000, Message: "resolve volume formatter: " + err.Error()}
			}
			formatter = f
		}
		label := sp.Label
		if label == "" {
			label = volume.SanitizeLabel(filepath.Base(filepath.Dir(sp.DiskPath)))
		}
		if err := s.volumeUnused(sp.DiskPath); err != nil {
			return &api.RPCError{Code: -32000, Message: err.Error()}
		}
		if err := volume.EnsureFormatted(ctx, sp.DiskPath, label, 0, formatter); err != nil {
			return &api.RPCError{Code: -32000, Message: err.Error()}
		}
	}
	return nil
}

// volumeMountsFromSpec converts API wire types to vm domain types.
func volumeMountsFromSpec(specs []api.VolumeMountSpec) []vm.VolumeMount {
	out := make([]vm.VolumeMount, len(specs))
	for i, s := range specs {
		out[i] = vm.VolumeMount{
			DiskPath:  s.DiskPath,
			GuestPath: s.GuestPath,
			ReadOnly:  s.ReadOnly,
			Label:     s.Label,
		}
	}
	return out
}

// volumeMountsToSpec converts vm domain types to API wire types.
func volumeMountsToSpec(vols []vm.VolumeMount) []api.VolumeMountSpec {
	if len(vols) == 0 {
		return nil
	}
	out := make([]api.VolumeMountSpec, len(vols))
	for i, v := range vols {
		out[i] = api.VolumeMountSpec{
			DiskPath:  v.DiskPath,
			GuestPath: v.GuestPath,
			ReadOnly:  v.ReadOnly,
			Label:     v.Label,
		}
	}
	return out
}

// portMapsToSpec converts vm domain types to API wire types.
func portMapsToSpec(pms []vm.PortMap) []api.PortMapSpec {
	if len(pms) == 0 {
		return nil
	}
	out := make([]api.PortMapSpec, len(pms))
	for i, pm := range pms {
		out[i] = api.PortMapSpec{
			HostPort:  pm.HostPort,
			GuestPort: pm.GuestPort,
			Protocol:  string(pm.Protocol),
			BindAddr:  pm.BindAddr,
		}
	}
	return out
}

// parseSig converts a signal name ("SIGTERM", "15") to an os.Signal.
func parseSig(s string) (syscall.Signal, error) {
	sigMap := map[string]syscall.Signal{
		"SIGTERM": syscall.SIGTERM,
		"SIGINT":  syscall.SIGINT,
		"SIGKILL": syscall.SIGKILL,
		"SIGHUP":  syscall.SIGHUP,
		"SIGQUIT": syscall.SIGQUIT,
		"SIGUSR1": syscall.Signal(10),
		"SIGUSR2": syscall.Signal(12),
	}
	if sig, ok := sigMap[s]; ok {
		return sig, nil
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, fmt.Errorf("unknown signal %q", s)
	}
	return syscall.Signal(n), nil
}

func (s *Server) handleAttach(ctx context.Context, params json.RawMessage, conn net.Conn, reqID int64) {
	var p api.IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.writeError(conn, reqID, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()})
		return
	}
	v, err := s.mgr.Get(p.ID)
	if err != nil {
		s.writeError(conn, reqID, &api.RPCError{Code: -32000, Message: err.Error()})
		return
	}

	reader := v.AttachReader()
	if reader == nil {
		s.writeError(conn, reqID, &api.RPCError{Code: -32000, Message: "vm not started in attach mode"})
		return
	}

	// Send success response before streaming raw console data.
	resp := api.Response{JSONRPC: "2.0", ID: reqID}
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		return
	}

	buf := make([]byte, 4096)
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			if _, writeErr := conn.Write(buf[:n]); writeErr != nil {
				return
			}
		}
		if readErr != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func (s *Server) handleNetworkCreate(params json.RawMessage) (any, *api.RPCError) {
	var p api.NetworkCreateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	n, err := s.netStore.Create(p.Name, p.Subnet, p.Driver)
	if err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	return networkToInfo(n), nil
}

func (s *Server) handleNetworkList() (any, *api.RPCError) {
	nets, err := s.netStore.List()
	if err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	infos := make([]api.NetworkInfo, len(nets))
	for i, n := range nets {
		infos[i] = networkToInfo(n)
	}
	return infos, nil
}

func (s *Server) handleNetworkGet(params json.RawMessage) (any, *api.RPCError) {
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	n, err := s.netStore.Get(p.Name)
	if err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	return networkToInfo(n), nil
}

func (s *Server) handleNetworkRemove(params json.RawMessage) (any, *api.RPCError) {
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	// Refuse to remove a network that running VMs still use: Store.Remove now
	// tears down the Linux bridge, and doing that under a live VM would sever its
	// connectivity. Stopped VMs have already had their taps deleted, so only
	// running ones block removal.
	inUse := 0
	for _, v := range s.mgr.List() {
		if v.Cfg.NetworkName == p.Name && v.GetState() == vm.StateRunning {
			inUse++
		}
	}
	if inUse > 0 {
		return nil, &api.RPCError{Code: -32000, Message: fmt.Sprintf("network %q is in use by %d running VM(s); stop them first", p.Name, inUse)}
	}
	if err := s.netStore.Remove(p.Name); err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	return map[string]string{"status": "ok"}, nil
}

func (s *Server) handleNetworkAllocateIP(params json.RawMessage) (any, *api.RPCError) {
	var p struct {
		Network string `json:"network"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	ip, err := s.netStore.AllocateIP(p.Network)
	if err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	return map[string]string{"ip": ip.String()}, nil
}

func (s *Server) handleNetworkReleaseIP(params json.RawMessage) (any, *api.RPCError) {
	var p struct {
		Network string `json:"network"`
		IP      string `json:"ip"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if err := s.netStore.ReleaseIP(p.Network, p.IP); err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	return map[string]string{"status": "ok"}, nil
}

func networkToInfo(n *network.Network) api.NetworkInfo {
	driver := n.Driver
	if runtime.GOOS == "darwin" {
		driver = "userspace"
	}
	return api.NetworkInfo{
		Name:      n.Name,
		Driver:    driver,
		Subnet:    n.Subnet,
		Gateway:   n.Gateway,
		Bridge:    n.Bridge,
		CreatedAt: n.CreatedAt.Format(time.RFC3339),
	}
}

func (s *Server) handleDNSResolve(params json.RawMessage) (any, *api.RPCError) {
	var p api.DNSResolveParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	rec, err := s.resolver.Resolve(p.Name, p.Network)
	if err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	return api.DNSRecord{Name: rec.Name, Network: rec.Network, IP: rec.IP, VMID: rec.VMID}, nil
}

func (s *Server) handleDNSList(params json.RawMessage) (any, *api.RPCError) {
	var p struct {
		Network string `json:"network,omitempty"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
		}
	}
	recs := s.resolver.List(p.Network)
	out := make([]api.DNSRecord, len(recs))
	for i, rec := range recs {
		out[i] = api.DNSRecord{Name: rec.Name, Network: rec.Network, IP: rec.IP, VMID: rec.VMID}
	}
	return out, nil
}

func (s *Server) handleNodeList() (any, *api.RPCError) {
	if s.cluster == nil {
		// Cluster mode is an application-level feature toggle, not a protocol
		// mismatch, so return an app-range code (no "(rpc -32601)" suffix) with
		// actionable guidance instead of a raw method-not-found (E2E follow-up).
		return nil, &api.RPCError{Code: -32000, Message: "Node.List unavailable: cluster is disabled; start the daemon with --cluster-addr to enable it"}
	}
	members := s.cluster.Members()
	rows := make([]api.NodeRow, len(members))
	for i, m := range members {
		rows[i] = api.NodeRow{
			ID:       m.ID,
			Addr:     m.Addr,
			Status:   m.Status,
			VMCount:  m.VMCount,
			CPUCap:   m.CPUCap,
			MemCap:   m.MemCap,
			LastSeen: m.LastSeen.Format(time.RFC3339),
		}
	}
	return api.NodeListResponse{Nodes: rows}, nil
}

func (s *Server) handleDNSResolveAll(params json.RawMessage) (any, *api.RPCError) {
	var p api.DNSResolveParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	recs, err := s.resolver.ResolveAll(p.Name, p.Network)
	if err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	out := make([]api.DNSRecord, len(recs))
	for i, rec := range recs {
		out[i] = api.DNSRecord{Name: rec.Name, Network: rec.Network, IP: rec.IP, VMID: rec.VMID}
	}
	return out, nil
}

func (s *Server) writeError(conn net.Conn, id int64, rpcErr *api.RPCError) {
	resp := api.Response{JSONRPC: "2.0", ID: id, Error: rpcErr}
	_ = json.NewEncoder(conn).Encode(resp)
}
