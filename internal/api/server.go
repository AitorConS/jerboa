package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/image"
	"github.com/AitorConS/unikernel-engine/internal/network"
	"github.com/AitorConS/unikernel-engine/internal/scheduler"
	"github.com/AitorConS/unikernel-engine/internal/service"
	"github.com/AitorConS/unikernel-engine/internal/vm"
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
	mgr        vm.Manager
	netStore   *network.Store
	svcMgr     *service.Manager
	listener   net.Listener
	shutdownFn func()
	version    string
	resolver   *scheduler.Resolver
	cluster    ClusterMemberLister
	imgStore   *image.Store
	mkfsMu     sync.RWMutex
	mkfs       image.MkfsFunc
}

// NewServer creates a Server that will listen on endpoint. The endpoint may
// carry a scheme (unix:// or tcp://); a bare value is treated as a Unix socket
// path. For unix endpoints any stale socket file is removed before binding.
// shutdownFn is called (in a goroutine) when a Daemon.Shutdown RPC is received;
// pass nil to disable remote shutdown.
// version is returned by Daemon.Version RPC; pass "" if unknown.
func NewServer(mgr vm.Manager, netStore *network.Store, svcMgr *service.Manager, endpoint string, shutdownFn func(), version string, clusterLister ClusterMemberLister) (*Server, error) {
	l, err := listen(endpoint)
	if err != nil {
		return nil, err
	}
	return &Server{mgr: mgr, netStore: netStore, svcMgr: svcMgr, listener: l, shutdownFn: shutdownFn, version: version, resolver: scheduler.NewResolver(mgr), cluster: clusterLister}, nil
}

// listen binds a net.Listener for the given endpoint, clearing stale Unix
// sockets first.
func listen(endpoint string) (net.Listener, error) {
	network, address, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if network == "unix" {
		if err := os.Remove(address); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("api server remove stale socket: %w", err)
		}
	}
	l, err := net.Listen(network, address) //nolint:noctx // server setup; lifecycle managed by Serve's ctx
	if err != nil {
		return nil, fmt.Errorf("api server listen %s: %w", endpoint, err)
	}
	return l, nil
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
	defer func() {
		if err := conn.Close(); err != nil {
			slog.Warn("api server close conn", "err", err)
		}
	}()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for dec.More() {
		var req Request
		if err := dec.Decode(&req); err != nil {
			return
		}
		result, rpcErr := s.dispatch(ctx, &req, conn, dec)
		if result == attachHandled {
			return
		}
		resp := Response{JSONRPC: "2.0", ID: req.ID}
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

// attachHandled is a sentinel value returned by dispatch when VM.Attach
// has taken over the connection and no response should be sent.
var attachHandled = struct{}{}

func (s *Server) dispatch(ctx context.Context, req *Request, conn net.Conn, dec *json.Decoder) (any, *RPCError) {
	switch req.Method {
	case "Image.Build":
		// Build streams its context after the request. Read the decoder's
		// leftover buffer first, then the raw connection. json.Encoder writes a
		// trailing newline after the request that the decoder leaves buffered,
		// so skip any leading whitespace before the binary frame stream.
		br := bufio.NewReader(io.MultiReader(dec.Buffered(), conn))
		skipLeadingWhitespace(br)
		s.handleBuild(ctx, req.Params, newFrameReader(br), conn, req.ID)
		return attachHandled, nil
	case "Image.List":
		return s.handleImageList()
	case "Image.Remove":
		return s.handleImageRemove(req.Params)
	case "VM.Run":
		return s.handleRun(ctx, req.Params)
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
	case "Service.Run":
		return s.handleServiceRun(ctx, req.Params)
	case "Service.Scale":
		return s.handleServiceScale(ctx, req.Params)
	case "Service.Update":
		return s.handleServiceUpdate(ctx, req.Params)
	case "Service.List":
		return s.handleServiceList()
	case "Service.Get":
		return s.handleServiceGet(req.Params)
	case "Service.Remove":
		return s.handleServiceRemove(ctx, req.Params)
	case "DNS.ResolveAll":
		return s.handleDNSResolveAll(req.Params)
	default:
		return nil, &RPCError{Code: -32601, Message: "method not found: " + req.Method}
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
		return "", fmt.Errorf("image %q not found: %w", image, err)
	}
	return diskPath, nil
}

// looksLikePath reports whether s is a filesystem path rather than a name:tag
// image reference. name:tag values contain a ':' but no path separators and no
// Windows drive prefix.
func looksLikePath(s string) bool {
	return strings.ContainsAny(s, "/\\") || (len(s) >= 2 && s[1] == ':')
}

func (s *Server) handleRun(ctx context.Context, params json.RawMessage) (any, *RPCError) {
	var p RunParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	imagePath := p.ImagePath
	if p.Image != "" {
		resolved, rerr := s.resolveImageRef(p.Image)
		if rerr != nil {
			return nil, &RPCError{Code: -32000, Message: rerr.Error()}
		}
		imagePath = resolved
	}
	cfg := vm.Config{
		ImagePath:   imagePath,
		Memory:      p.Memory,
		CPUs:        p.CPUs,
		NetworkName: p.NetworkName,
		PortMaps:    portMapsFromSpec(p.PortMaps),
		Env:         p.Env,
		Name:        p.Name,
		Volumes:     volumeMountsFromSpec(p.Volumes),
		Attach:      p.Attach,
		IPAddress:   p.IPAddress,
		GatewayIP:   p.GatewayIP,
		BridgeName:  p.BridgeName,
		SubnetMask:  p.SubnetMask,
		CPUShares:   p.CPUShares,
		MemoryMax:   p.MemoryMax,
		DiskIOPS:    p.DiskIOPS,
		DiskBPS:     p.DiskBPS,
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
	v, err := s.mgr.Create(ctx, cfg)
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	if err := s.mgr.Start(ctx, v.ID); err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
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

func (s *Server) handleStop(ctx context.Context, params json.RawMessage) (any, *RPCError) {
	var p StopParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	var err error
	if p.Force {
		err = s.mgr.Kill(ctx, p.ID)
	} else {
		err = s.mgr.Stop(ctx, p.ID)
	}
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	return map[string]string{"status": "ok"}, nil
}

func (s *Server) handleKill(ctx context.Context, params json.RawMessage) (any, *RPCError) {
	var p IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if err := s.mgr.Kill(ctx, p.ID); err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	return map[string]string{"status": "ok"}, nil
}

func (s *Server) handleSignal(ctx context.Context, params json.RawMessage) (any, *RPCError) {
	var p SignalParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	sig, err := parseSig(p.Signal)
	if err != nil {
		return nil, &RPCError{Code: -32602, Message: err.Error()}
	}
	if err := s.mgr.Signal(ctx, p.ID, sig); err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	return map[string]string{"status": "ok"}, nil
}

func (s *Server) handleRemove(ctx context.Context, params json.RawMessage) (any, *RPCError) {
	var p IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if err := s.mgr.Remove(ctx, p.ID); err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	return map[string]string{"status": "ok"}, nil
}

func (s *Server) handleList(_ context.Context) (any, *RPCError) {
	vms := s.mgr.List()
	infos := make([]VMInfo, len(vms))
	for i, v := range vms {
		infos[i] = toInfo(v)
	}
	return infos, nil
}

func (s *Server) handleGet(params json.RawMessage) (any, *RPCError) {
	var p IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	v, err := s.mgr.Get(p.ID)
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	return toInfo(v), nil
}

func (s *Server) handleLogs(params json.RawMessage) (any, *RPCError) {
	var p IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	v, err := s.mgr.Get(p.ID)
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	return LogsResponse{ID: v.ID, Logs: string(v.Logs())}, nil
}

func (s *Server) handleDaemonShutdown() (any, *RPCError) {
	if s.shutdownFn != nil {
		go s.shutdownFn()
	}
	return map[string]string{"status": "ok"}, nil
}

func (s *Server) handleDaemonVersion() (any, *RPCError) {
	return map[string]string{"version": s.version}, nil
}

func (s *Server) handleInspect(params json.RawMessage) (any, *RPCError) {
	var p IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	v, err := s.mgr.Get(p.ID)
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	return toDetail(v), nil
}

func (s *Server) handleStats(params json.RawMessage) (any, *RPCError) {
	var p IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	v, err := s.mgr.Get(p.ID)
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	stats := v.Stats()
	return VMStatsResponse{
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

func toInfo(v *vm.VM) VMInfo {
	return VMInfo{
		ID:     v.ID,
		State:  string(v.GetState()),
		Image:  v.Cfg.ImagePath,
		Name:   v.Cfg.Name,
		Health: string(v.GetHealthStatus()),
	}
}

func toDetail(v *vm.VM) VMDetail {
	d := VMDetail{
		ID:              v.ID,
		State:           string(v.GetState()),
		Image:           v.Cfg.ImagePath,
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
func portMapsFromSpec(specs []PortMapSpec) []vm.PortMap {
	out := make([]vm.PortMap, len(specs))
	for i, s := range specs {
		out[i] = vm.PortMap{
			HostPort:  s.HostPort,
			GuestPort: s.GuestPort,
			Protocol:  vm.PortProtocol(s.Protocol),
		}
	}
	return out
}

// volumeMountsFromSpec converts API wire types to vm domain types.
func volumeMountsFromSpec(specs []VolumeMountSpec) []vm.VolumeMount {
	out := make([]vm.VolumeMount, len(specs))
	for i, s := range specs {
		out[i] = vm.VolumeMount{
			DiskPath:  s.DiskPath,
			GuestPath: s.GuestPath,
			ReadOnly:  s.ReadOnly,
		}
	}
	return out
}

// volumeMountsToSpec converts vm domain types to API wire types.
func volumeMountsToSpec(vols []vm.VolumeMount) []VolumeMountSpec {
	if len(vols) == 0 {
		return nil
	}
	out := make([]VolumeMountSpec, len(vols))
	for i, v := range vols {
		out[i] = VolumeMountSpec{
			DiskPath:  v.DiskPath,
			GuestPath: v.GuestPath,
			ReadOnly:  v.ReadOnly,
		}
	}
	return out
}

// portMapsToSpec converts vm domain types to API wire types.
func portMapsToSpec(pms []vm.PortMap) []PortMapSpec {
	if len(pms) == 0 {
		return nil
	}
	out := make([]PortMapSpec, len(pms))
	for i, pm := range pms {
		out[i] = PortMapSpec{
			HostPort:  pm.HostPort,
			GuestPort: pm.GuestPort,
			Protocol:  string(pm.Protocol),
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
	var p IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.writeError(conn, reqID, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()})
		return
	}
	v, err := s.mgr.Get(p.ID)
	if err != nil {
		s.writeError(conn, reqID, &RPCError{Code: -32000, Message: err.Error()})
		return
	}

	reader := v.AttachReader()
	if reader == nil {
		s.writeError(conn, reqID, &RPCError{Code: -32000, Message: "vm not started in attach mode"})
		return
	}

	// Send success response before streaming raw console data.
	resp := Response{JSONRPC: "2.0", ID: reqID}
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

func (s *Server) handleNetworkCreate(params json.RawMessage) (any, *RPCError) {
	var p NetworkCreateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	n, err := s.netStore.Create(p.Name, p.Subnet, p.Driver)
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	return networkToInfo(n), nil
}

func (s *Server) handleNetworkList() (any, *RPCError) {
	nets, err := s.netStore.List()
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	infos := make([]NetworkInfo, len(nets))
	for i, n := range nets {
		infos[i] = networkToInfo(n)
	}
	return infos, nil
}

func (s *Server) handleNetworkGet(params json.RawMessage) (any, *RPCError) {
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	n, err := s.netStore.Get(p.Name)
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	return networkToInfo(n), nil
}

func (s *Server) handleNetworkRemove(params json.RawMessage) (any, *RPCError) {
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if err := s.netStore.Remove(p.Name); err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	return map[string]string{"status": "ok"}, nil
}

func (s *Server) handleNetworkAllocateIP(params json.RawMessage) (any, *RPCError) {
	var p struct {
		Network string `json:"network"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	ip, err := s.netStore.AllocateIP(p.Network)
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	return map[string]string{"ip": ip.String()}, nil
}

func (s *Server) handleNetworkReleaseIP(params json.RawMessage) (any, *RPCError) {
	var p struct {
		Network string `json:"network"`
		IP      string `json:"ip"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if err := s.netStore.ReleaseIP(p.Network, p.IP); err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	return map[string]string{"status": "ok"}, nil
}

func networkToInfo(n *network.Network) NetworkInfo {
	return NetworkInfo{
		Name:      n.Name,
		Driver:    n.Driver,
		Subnet:    n.Subnet,
		Gateway:   n.Gateway,
		Bridge:    n.Bridge,
		CreatedAt: n.CreatedAt.Format(time.RFC3339),
	}
}

func (s *Server) handleDNSResolve(params json.RawMessage) (any, *RPCError) {
	var p DNSResolveParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	rec, err := s.resolver.Resolve(p.Name, p.Network)
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	return DNSRecord{Name: rec.Name, Network: rec.Network, IP: rec.IP, VMID: rec.VMID}, nil
}

func (s *Server) handleDNSList(params json.RawMessage) (any, *RPCError) {
	var p struct {
		Network string `json:"network,omitempty"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
		}
	}
	recs := s.resolver.List(p.Network)
	out := make([]DNSRecord, len(recs))
	for i, rec := range recs {
		out[i] = DNSRecord{Name: rec.Name, Network: rec.Network, IP: rec.IP, VMID: rec.VMID}
	}
	return out, nil
}

func (s *Server) handleNodeList() (any, *RPCError) {
	if s.cluster == nil {
		return nil, &RPCError{Code: -32601, Message: "method not found: Node.List (cluster disabled)"}
	}
	members := s.cluster.Members()
	rows := make([]NodeRow, len(members))
	for i, m := range members {
		rows[i] = NodeRow{
			ID:       m.ID,
			Addr:     m.Addr,
			Status:   m.Status,
			VMCount:  m.VMCount,
			CPUCap:   m.CPUCap,
			MemCap:   m.MemCap,
			LastSeen: m.LastSeen.Format(time.RFC3339),
		}
	}
	return NodeListResponse{Nodes: rows}, nil
}

func (s *Server) handleServiceRun(ctx context.Context, params json.RawMessage) (any, *RPCError) {
	if s.svcMgr == nil {
		return nil, &RPCError{Code: -32601, Message: "method not found: Service.Run (service manager disabled)"}
	}
	var p ServiceRunParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	opts := service.Options{
		Memory:      p.Memory,
		CPUs:        p.CPUs,
		Env:         p.Env,
		NetworkName: p.NetworkName,
	}
	if p.HealthTimeout > 0 {
		opts.HealthTimeout = time.Duration(p.HealthTimeout) * time.Second
	}
	if p.HealthCheck != nil {
		opts.HealthCheck = &vm.HealthCheckConfig{
			Type:     p.HealthCheck.Type,
			Port:     p.HealthCheck.Port,
			Path:     p.HealthCheck.Path,
			Interval: time.Duration(p.HealthCheck.Interval) * time.Second,
			Timeout:  time.Duration(p.HealthCheck.Timeout) * time.Second,
			Retries:  p.HealthCheck.Retries,
		}
	}
	if p.Restart != nil {
		opts.Restart = vm.RestartConfig{
			Policy:     vm.RestartPolicy(p.Restart.Policy),
			MaxRetries: p.Restart.MaxRetries,
		}
	}
	if len(p.PortMaps) > 0 {
		opts.PortMaps = portMapsFromSpec(p.PortMaps)
	}
	if p.Strategy != "" {
		opts.Strategy = service.Strategy(p.Strategy)
	}
	imagePath, rerr := s.resolveImageRef(p.Image)
	if rerr != nil {
		return nil, &RPCError{Code: -32000, Message: rerr.Error()}
	}
	svc, err := s.svcMgr.Run(ctx, p.Name, imagePath, p.Replicas, opts)
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	svcInfo := s.svcMgr.ServiceInfo(svc)
	return ServiceInfoResult{
		Name:            svcInfo.Name,
		Image:           svcInfo.Image,
		DesiredReplicas: svcInfo.DesiredReplicas,
		ReadyReplicas:   svcInfo.ReadyReplicas,
		Strategy:        svcInfo.Strategy,
		Health:          svcInfo.Health,
		Env:             svcInfo.Env,
		CreatedAt:       svcInfo.CreatedAt,
		UpdatedAt:       svcInfo.UpdatedAt,
		ReplicaIDs:      svcInfo.ReplicaIDs,
	}, nil
}

func (s *Server) handleServiceScale(ctx context.Context, params json.RawMessage) (any, *RPCError) {
	if s.svcMgr == nil {
		return nil, &RPCError{Code: -32601, Message: "method not found: Service.Scale (service manager disabled)"}
	}
	var p ServiceScaleParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	svc, err := s.svcMgr.Scale(ctx, p.Name, p.DesiredReplicas)
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	svcInfo := s.svcMgr.ServiceInfo(svc)
	return ServiceInfoResult{
		Name:            svcInfo.Name,
		Image:           svcInfo.Image,
		DesiredReplicas: svcInfo.DesiredReplicas,
		ReadyReplicas:   svcInfo.ReadyReplicas,
		Strategy:        svcInfo.Strategy,
		Health:          svcInfo.Health,
		Env:             svcInfo.Env,
		CreatedAt:       svcInfo.CreatedAt,
		UpdatedAt:       svcInfo.UpdatedAt,
		ReplicaIDs:      svcInfo.ReplicaIDs,
	}, nil
}

func (s *Server) handleServiceUpdate(ctx context.Context, params json.RawMessage) (any, *RPCError) {
	if s.svcMgr == nil {
		return nil, &RPCError{Code: -32601, Message: "method not found: Service.Update (service manager disabled)"}
	}
	var p ServiceUpdateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	imagePath, rerr := s.resolveImageRef(p.Image)
	if rerr != nil {
		return nil, &RPCError{Code: -32000, Message: rerr.Error()}
	}
	svc, err := s.svcMgr.Update(ctx, p.Name, imagePath, time.Duration(p.HealthTimeout)*time.Second)
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	svcInfo := s.svcMgr.ServiceInfo(svc)
	return ServiceInfoResult{
		Name:            svcInfo.Name,
		Image:           svcInfo.Image,
		DesiredReplicas: svcInfo.DesiredReplicas,
		ReadyReplicas:   svcInfo.ReadyReplicas,
		Strategy:        svcInfo.Strategy,
		Health:          svcInfo.Health,
		Env:             svcInfo.Env,
		CreatedAt:       svcInfo.CreatedAt,
		UpdatedAt:       svcInfo.UpdatedAt,
		ReplicaIDs:      svcInfo.ReplicaIDs,
	}, nil
}

func (s *Server) handleServiceList() (any, *RPCError) {
	if s.svcMgr == nil {
		return nil, &RPCError{Code: -32601, Message: "method not found: Service.List (service manager disabled)"}
	}
	services, err := s.svcMgr.List()
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	out := make([]ServiceInfoResult, len(services))
	for i, svc := range services {
		svcInfo := s.svcMgr.ServiceInfo(svc)
		out[i] = ServiceInfoResult{
			Name:            svcInfo.Name,
			Image:           svcInfo.Image,
			DesiredReplicas: svcInfo.DesiredReplicas,
			ReadyReplicas:   svcInfo.ReadyReplicas,
			Strategy:        svcInfo.Strategy,
			Health:          svcInfo.Health,
			Env:             svcInfo.Env,
			CreatedAt:       svcInfo.CreatedAt,
			UpdatedAt:       svcInfo.UpdatedAt,
			ReplicaIDs:      svcInfo.ReplicaIDs,
		}
	}
	return out, nil
}

func (s *Server) handleServiceGet(params json.RawMessage) (any, *RPCError) {
	if s.svcMgr == nil {
		return nil, &RPCError{Code: -32601, Message: "method not found: Service.Get (service manager disabled)"}
	}
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	svc, err := s.svcMgr.Get(p.Name)
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	svcInfo := s.svcMgr.ServiceInfo(svc)
	return ServiceInfoResult{
		Name:            svcInfo.Name,
		Image:           svcInfo.Image,
		DesiredReplicas: svcInfo.DesiredReplicas,
		ReadyReplicas:   svcInfo.ReadyReplicas,
		Strategy:        svcInfo.Strategy,
		Health:          svcInfo.Health,
		Env:             svcInfo.Env,
		CreatedAt:       svcInfo.CreatedAt,
		UpdatedAt:       svcInfo.UpdatedAt,
		ReplicaIDs:      svcInfo.ReplicaIDs,
	}, nil
}

func (s *Server) handleServiceRemove(ctx context.Context, params json.RawMessage) (any, *RPCError) {
	if s.svcMgr == nil {
		return nil, &RPCError{Code: -32601, Message: "method not found: Service.Remove (service manager disabled)"}
	}
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if err := s.svcMgr.Remove(ctx, p.Name); err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	return map[string]string{"status": "ok"}, nil
}

func (s *Server) handleDNSResolveAll(params json.RawMessage) (any, *RPCError) {
	var p DNSResolveParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	recs, err := s.resolver.ResolveAll(p.Name, p.Network)
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	out := make([]DNSRecord, len(recs))
	for i, rec := range recs {
		out[i] = DNSRecord{Name: rec.Name, Network: rec.Network, IP: rec.IP, VMID: rec.VMID}
	}
	return out, nil
}

func (s *Server) writeError(conn net.Conn, id int64, rpcErr *RPCError) {
	resp := Response{JSONRPC: "2.0", ID: id, Error: rpcErr}
	_ = json.NewEncoder(conn).Encode(resp)
}
