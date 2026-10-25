package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/config"
	"github.com/AitorConS/jerboa/internal/preflight"
	"github.com/AitorConS/jerboa/internal/wslboot"
	"github.com/AitorConS/jerboa/internal/wsldistro"
)

type Config struct {
	Listen      string
	Endpoint    string
	Token       string
	Version     string
	Distro      string
	User        string
	Sudo        bool
	Hypervisor  string
	JerboadPath string
}

func ResolveEndpoint(override string) (string, error) {
	endpoint := config.ResolveEndpoint(override)
	if runtime.GOOS != "windows" || override != "" || !strings.HasPrefix(endpoint, "tcp://") {
		return endpoint, nil
	}
	ip, err := wsldistro.IP()
	if err != nil {
		return "", err
	}
	return "tcp://" + net.JoinHostPort(ip, daemonPort(endpoint)), nil
}

func DefaultWindowsDistro() string { return wsldistro.Name }

type Server struct {
	cfg    Config
	mux    *http.ServeMux
	poller *EventPoller
}

func NewServer(cfg Config) *Server {
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.poller = NewEventPoller(func() (VMLister, func(), error) {
		c, err := s.dial()
		if err != nil {
			return nil, nil, err
		}
		return c, func() { _ = c.Close() }, nil
	})
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return AuthMiddleware(s.cfg.Token, s.mux)
}

func (s *Server) Listen() (net.Listener, error) {
	addr := s.cfg.Listen
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("agent listen: %w", err)
	}
	return ln, nil
}

func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	httpSrv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = httpSrv.Shutdown(context.Background())
	}()
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("agent serve: %w", err)
	}
	return nil
}

func (s *Server) dial() (*api.Client, error) {
	c, err := api.DialWithToken(s.cfg.Endpoint, s.daemonToken())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errDaemonUnreachable, err)
	}
	return c, nil
}

func (s *Server) daemonToken() string {
	if tok := config.ResolveToken(); tok != "" {
		return tok
	}
	token, _, err := wslboot.LoadDaemonFile(config.DaemonFilePath())
	if err == nil {
		return token
	}
	return ""
}

// withClient dials the daemon and runs fn under the inbound request's context,
// so a hung daemon call is abandoned when the HTTP client disconnects instead
// of blocking the handler goroutine forever.
func (s *Server) withClient(w http.ResponseWriter, r *http.Request, fn func(context.Context, *api.Client) (any, error)) {
	c, err := s.dial()
	if err != nil {
		WriteMappedError(w, err)
		return
	}
	defer func() { _ = c.Close() }()
	out, err := fn(r.Context(), c)
	if err != nil {
		WriteMappedError(w, err)
		return
	}
	writeJSON(w, out)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /v1/preflight", s.handlePreflight)
	s.mux.HandleFunc("POST /v1/daemon/ensure", s.handleDaemonEnsure)
	s.mux.HandleFunc("POST /v1/daemon/start", s.handleDaemonStart)
	s.mux.HandleFunc("POST /v1/daemon/stop", s.handleDaemonStop)
	s.mux.HandleFunc("GET /v1/daemon/status", s.handleDaemonStatus)
	s.mux.HandleFunc("GET /v1/vms", s.handleVMList)
	s.mux.HandleFunc("POST /v1/vms", s.handleVMRun)
	s.mux.HandleFunc("GET /v1/vms/{id}", s.handleVMGet)
	s.mux.HandleFunc("GET /v1/vms/{id}/inspect", s.handleVMInspect)
	s.mux.HandleFunc("GET /v1/vms/{id}/logs", s.handleVMLogs)
	s.mux.HandleFunc("GET /v1/vms/{id}/stats", s.handleVMStats)
	s.mux.HandleFunc("GET /v1/vms/{id}/attach", s.handleVMAttach)
	s.mux.HandleFunc("POST /v1/vms/{id}/stop", s.handleVMStop)
	s.mux.HandleFunc("POST /v1/vms/{id}/kill", s.handleVMKill)
	s.mux.HandleFunc("POST /v1/vms/{id}/signal", s.handleVMSignal)
	s.mux.HandleFunc("DELETE /v1/vms/{id}", s.handleVMRemove)
	s.mux.HandleFunc("GET /v1/images", s.handleImageList)
	s.mux.HandleFunc("GET /v1/images/{ref...}", s.handleImageGet)
	s.mux.HandleFunc("DELETE /v1/images/{ref...}", s.handleImageRemove)
	s.mux.HandleFunc("GET /v1/networks", s.handleNetworkList)
	s.mux.HandleFunc("POST /v1/networks", s.handleNetworkCreate)
	s.mux.HandleFunc("GET /v1/networks/{name}", s.handleNetworkGet)
	s.mux.HandleFunc("DELETE /v1/networks/{name}", s.handleNetworkRemove)
	s.mux.HandleFunc("GET /v1/dns", s.handleDNSList)
	s.mux.HandleFunc("GET /v1/dns/resolve", s.handleDNSResolve)
	s.mux.HandleFunc("GET /v1/nodes", s.handleNodes)
	s.mux.HandleFunc("GET /v1/events", s.handleEvents)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"agentVersion": s.cfg.Version, "daemonReachable": false, "daemonVersion": ""}
	c, err := s.dial()
	if err != nil {
		writeJSON(w, resp)
		return
	}
	defer func() { _ = c.Close() }()
	ver, err := c.DaemonVersion(r.Context())
	if err == nil {
		resp["daemonReachable"] = true
		resp["daemonVersion"] = ver
	}
	writeJSON(w, resp)
}

func (s *Server) handlePreflight(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string][]preflight.Finding{"findings": {}})
}

func (s *Server) handleDaemonEnsure(w http.ResponseWriter, r *http.Request) {
	if err := s.ensureDaemon(r.Context()); err != nil {
		WriteMappedError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleDaemonStart(w http.ResponseWriter, r *http.Request) {
	if err := s.startDaemon(r.Context()); err != nil {
		WriteMappedError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleDaemonStop(w http.ResponseWriter, _ *http.Request) {
	if runtime.GOOS != "windows" {
		WriteError(w, http.StatusBadRequest, KindNotSupported, "daemon stop is only supported on Windows")
		return
	}
	if err := wslboot.Stop(s.cfg.Distro, s.cfg.User); err != nil && !errors.Is(err, wslboot.ErrNoDaemon) {
		WriteMappedError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleDaemonStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"reachable": false, "version": ""}
	c, err := s.dial()
	if err != nil {
		writeJSON(w, resp)
		return
	}
	defer func() { _ = c.Close() }()
	ver, err := c.DaemonVersion(r.Context())
	if err == nil {
		resp["reachable"] = true
		resp["version"] = ver
	}
	writeJSON(w, resp)
}

func (s *Server) ensureDaemon(ctx context.Context) error {
	if s.isReachable(ctx) {
		return nil
	}
	if runtime.GOOS != "windows" {
		return fmt.Errorf("daemon ensure is not supported on %s: %w", runtime.GOOS, errNotSupported)
	}
	cfg, token, err := s.launchConfig()
	if err != nil {
		return err
	}
	if err := wslboot.EnsureDaemon(ctx, cfg); err != nil {
		return err
	}
	return wslboot.SaveDaemonFile(config.DaemonFilePath(), token, cfg.Endpoint)
}

func (s *Server) startDaemon(ctx context.Context) error {
	if s.isReachable(ctx) {
		return nil
	}
	if runtime.GOOS != "windows" {
		return fmt.Errorf("daemon start is not supported on %s: %w", runtime.GOOS, errNotSupported)
	}
	cfg, token, err := s.launchConfig()
	if err != nil {
		return err
	}
	if err := wslboot.Launch(cfg); err != nil {
		return err
	}
	if err := wslboot.WaitHealthy(ctx, cfg); err != nil {
		return err
	}
	return wslboot.SaveDaemonFile(config.DaemonFilePath(), token, cfg.Endpoint)
}

var errNotSupported = errors.New("not supported")

func (s *Server) isReachable(ctx context.Context) bool {
	return wslboot.Healthy(ctx, s.cfg.Endpoint, s.daemonToken())
}

func (s *Server) launchConfig() (wslboot.Config, string, error) {
	token := s.daemonToken()
	if token == "" {
		t, err := wslboot.LoadOrCreateToken(config.DaemonFilePath())
		if err != nil {
			return wslboot.Config{}, "", fmt.Errorf("daemon token: %w", err)
		}
		token = t
		_ = os.Setenv("JERBOA_AUTH_TOKEN", token)
	}
	cfg := s.wslConfig(token)
	return cfg, token, nil
}

func (s *Server) wslConfig(token string) wslboot.Config {
	listenEndpoint := ""
	if strings.HasPrefix(s.cfg.Endpoint, "tcp://") {
		listenEndpoint = "tcp://0.0.0.0:" + daemonPort(s.cfg.Endpoint)
	}
	return wslboot.Config{
		Endpoint:       s.cfg.Endpoint,
		ListenEndpoint: listenEndpoint,
		Distro:         s.cfg.Distro,
		User:           s.cfg.User,
		Token:          token,
		JerboadPath:    s.cfg.JerboadPath,
		Hypervisor:     s.cfg.Hypervisor,
		Sudo:           s.cfg.Sudo,
		// Cold WSL2 boots regularly exceed the 20s wslboot default; the
		// desktop app shows progress, so waiting longer beats a false failure.
		HealthTimeout: 60 * time.Second,
	}
}

func daemonPort(endpoint string) string {
	rest, ok := strings.CutPrefix(endpoint, "tcp://")
	if !ok {
		return "7890"
	}
	_, port, err := net.SplitHostPort(rest)
	if err == nil && port != "" {
		return port
	}
	if _, port, found := strings.Cut(rest, ":"); found && port != "" {
		return port
	}
	return "7890"
}

func (s *Server) handleVMList(w http.ResponseWriter, r *http.Request) {
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) { return c.List(ctx) })
}

func (s *Server) handleVMRun(w http.ResponseWriter, r *http.Request) {
	var p api.RunParams
	if !decodeBody(w, r, &p) {
		return
	}
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) { return c.Run(ctx, p) })
}

func (s *Server) handleVMGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) { return c.Get(ctx, id) })
}

func (s *Server) handleVMInspect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) { return c.Inspect(ctx, id) })
}

func (s *Server) handleVMLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) { return c.Logs(ctx, id) })
}

func (s *Server) handleVMStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) { return c.Stats(ctx, id) })
}

func (s *Server) handleVMStop(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Force bool `json:"force"`
	}
	if !decodeBody(w, r, &p) {
		return
	}
	id := r.PathValue("id")
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) {
		return map[string]string{"status": "ok"}, c.Stop(ctx, id, p.Force)
	})
}

func (s *Server) handleVMKill(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) {
		return map[string]string{"status": "ok"}, c.Kill(ctx, id)
	})
}

func (s *Server) handleVMSignal(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Signal string `json:"signal"`
	}
	if !decodeBody(w, r, &p) {
		return
	}
	id := r.PathValue("id")
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) {
		return map[string]string{"status": "ok"}, c.Signal(ctx, id, p.Signal)
	})
}

func (s *Server) handleVMRemove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) {
		return map[string]string{"status": "ok"}, c.Remove(ctx, id)
	})
}

func (s *Server) handleVMAttach(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, http.StatusInternalServerError, KindInternal, "streaming unsupported")
		return
	}
	c, err := s.dial()
	if err != nil {
		WriteMappedError(w, err)
		return
	}
	defer func() { _ = c.Close() }()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()
	done := make(chan error, 1)
	go func() {
		done <- c.Attach(r.Context(), r.PathValue("id"), &sseChunkWriter{w: w, flusher: flusher})
	}()
	select {
	case <-r.Context().Done():
		// Closing the client unblocks the Attach goroutine; wait for it so no
		// goroutine writes to w (undefined behavior) after this handler returns.
		_ = c.Close()
		<-done
	case err := <-done:
		if err != nil {
			_ = WriteSSEJSON(w, "error", ErrorDetail{Message: err.Error(), Kind: KindInternal})
			flusher.Flush()
		}
	}
}

func (s *Server) handleImageList(w http.ResponseWriter, r *http.Request) {
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) { return c.ImageList(ctx) })
}

func (s *Server) handleImageGet(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) { return c.ImageGet(ctx, ref) })
}

func (s *Server) handleImageRemove(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) {
		return map[string]string{"status": "ok"}, c.ImageRemove(ctx, ref)
	})
}

func (s *Server) handleNetworkList(w http.ResponseWriter, r *http.Request) {
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) { return c.NetworkList(ctx) })
}

func (s *Server) handleNetworkCreate(w http.ResponseWriter, r *http.Request) {
	var p api.NetworkCreateParams
	if !decodeBody(w, r, &p) {
		return
	}
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) {
		return c.NetworkCreate(ctx, p.Name, p.Subnet, p.Driver)
	})
}

func (s *Server) handleNetworkGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) { return c.NetworkGet(ctx, name) })
}

func (s *Server) handleNetworkRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) {
		return map[string]string{"status": "ok"}, c.NetworkRemove(ctx, name)
	})
}

func (s *Server) handleDNSList(w http.ResponseWriter, r *http.Request) {
	network := r.URL.Query().Get("network")
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) { return c.DNSList(ctx, network) })
}

func (s *Server) handleDNSResolve(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	network := r.URL.Query().Get("network")
	all, _ := strconv.ParseBool(r.URL.Query().Get("all"))
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) {
		if all {
			return c.DNSResolveAll(ctx, name, network)
		}
		return c.DNSResolve(ctx, name, network)
	})
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	s.withClient(w, r, func(ctx context.Context, c *api.Client) (any, error) { return c.NodeList(ctx) })
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, http.StatusInternalServerError, KindInternal, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()
	for ev := range s.poller.Subscribe(r.Context()) {
		if len(ev.Data) > 0 && strings.HasPrefix(string(ev.Data), ":") {
			_, _ = fmt.Fprintf(w, "%s\n\n", ev.Data)
		} else {
			_ = WriteSSEEvent(w, ev.Name, ev.Data)
		}
		flusher.Flush()
	}
}

// maxBodyBytes bounds a request body; every agent request is a small JSON
// document.
const maxBodyBytes = 1 << 20

func decodeBody(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(out); err != nil {
		WriteError(w, http.StatusBadRequest, KindInvalid, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
