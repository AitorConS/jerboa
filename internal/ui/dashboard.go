package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/vm"
)

type DashboardData struct {
	VMs     []VMRow
	Total   int
	UIAddr  string
	Version string
}

type VMRow struct {
	ID     string
	Name   string
	State  string
	Image  string
	Health string
}

type VMDetailData struct {
	VM            VMDetailRow
	Logs          string
	UIAddr        string
	Version       string
	BackLink      string
}

type VMDetailRow struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	State         string    `json:"state"`
	Image         string    `json:"image"`
	Memory        string    `json:"memory"`
	CPUs          int       `json:"cpus"`
	Health        string    `json:"health"`
	RestartPolicy string    `json:"restart_policy"`
	RestartCount  int       `json:"restart_count"`
	CreatedAt     string    `json:"created_at"`
	StartedAt     string    `json:"started_at"`
	StoppedAt     string    `json:"stopped_at"`
	IPAddress     string    `json:"ip_address"`
	GatewayIP     string    `json:"gateway_ip"`
	Env           []string  `json:"env,omitempty"`
	Ports         []PortRow `json:"ports,omitempty"`
}

type PortRow struct {
	HostPort  uint16 `json:"host_port"`
	GuestPort uint16 `json:"guest_port"`
	Protocol  string `json:"protocol"`
}

type Handler struct {
	mgr          vm.Manager
	uiAddr       string
	dashTmpl     *template.Template
	detailTmpl   *template.Template
	version      string
}

func NewHandler(mgr vm.Manager, uiAddr, version string) *Handler {
	dashTmpl := template.Must(template.New("dashboard").Parse(dashboardTmpl))
	detailTmpl := template.Must(template.New("detail").Parse(detailTmpl))
	return &Handler{mgr: mgr, uiAddr: uiAddr, dashTmpl: dashTmpl, detailTmpl: detailTmpl, version: version}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if path == "/ui" || path == "/ui/" || path == "/" {
		h.serveDashboard(w, r)
		return
	}
	if path == "/ui/api/vms" {
		h.serveVMsJSON(w, r)
		return
	}
	if strings.HasPrefix(path, "/ui/api/vm/") && strings.HasSuffix(path, "/logs") {
		id := strings.TrimPrefix(path, "/ui/api/vm/")
		id = strings.TrimSuffix(id, "/logs")
		h.serveVMLogsJSON(w, r, id)
		return
	}
	if strings.HasPrefix(path, "/ui/api/vm/") {
		id := strings.TrimPrefix(path, "/ui/api/vm/")
		h.serveVMDetailJSON(w, r, id)
		return
	}
	if strings.HasPrefix(path, "/ui/vm/") {
		id := strings.TrimPrefix(path, "/ui/vm/")
		h.serveVMDetailPage(w, r, id)
		return
	}
	http.NotFound(w, r)
}

func (h *Handler) serveDashboard(w http.ResponseWriter, _ *http.Request) {
	vms := h.mgr.List()
	rows := make([]VMRow, len(vms))
	for i, v := range vms {
		name := v.Cfg.Name
		if name == "" {
			name = "-"
		}
		health := string(v.GetHealthStatus())
		if health == "" {
			health = "-"
		}
		rows[i] = VMRow{
			ID:     v.ID,
			Name:   name,
			State:  string(v.GetState()),
			Image:  v.Cfg.ImagePath,
			Health: health,
		}
	}
	data := DashboardData{
		VMs:     rows,
		Total:   len(rows),
		UIAddr:  h.uiAddr,
		Version: h.version,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.dashTmpl.Execute(w, data); err != nil {
		slog.Warn("dashboard template error", "err", err)
	}
}

func (h *Handler) serveVMDetailPage(w http.ResponseWriter, _ *http.Request, id string) {
	v, err := h.mgr.Get(id)
	if err != nil {
		http.NotFound(w, &http.Request{Method: http.MethodGet})
		return
	}
	row := vmToDetailRow(v)
	logs := string(v.Logs())
	data := VMDetailData{
		VM:       row,
		Logs:     logs,
		UIAddr:   h.uiAddr,
		Version:  h.version,
		BackLink: "/ui",
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.detailTmpl.Execute(w, data); err != nil {
		slog.Warn("detail template error", "err", err)
	}
}

func (h *Handler) serveVMsJSON(w http.ResponseWriter, _ *http.Request) {
	vms := h.mgr.List()
	type vmJSON struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		State  string `json:"state"`
		Image  string `json:"image"`
		Health string `json:"health"`
	}
	out := make([]vmJSON, len(vms))
	for i, v := range vms {
		name := v.Cfg.Name
		if name == "" {
			name = "-"
		}
		out[i] = vmJSON{
			ID:     v.ID,
			Name:   name,
			State:  string(v.GetState()),
			Image:  v.Cfg.ImagePath,
			Health: string(v.GetHealthStatus()),
		}
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(out)
}

func (h *Handler) serveVMDetailJSON(w http.ResponseWriter, _ *http.Request, id string) {
	v, err := h.mgr.Get(id)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "vm not found"})
		return
	}
	row := vmToDetailRow(v)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(row)
}

func (h *Handler) serveVMLogsJSON(w http.ResponseWriter, _ *http.Request, id string) {
	v, err := h.mgr.Get(id)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "vm not found"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"id":   v.ID,
		"logs": string(v.Logs()),
	})
}

func vmToDetailRow(v *vm.VM) VMDetailRow {
	name := v.Cfg.Name
	if name == "" {
		name = "-"
	}
	health := string(v.GetHealthStatus())
	if health == "" {
		health = "-"
	}
	ports := make([]PortRow, len(v.Cfg.PortMaps))
	for i, pm := range v.Cfg.PortMaps {
		ports[i] = PortRow{
			HostPort:  pm.HostPort,
			GuestPort: pm.GuestPort,
			Protocol:  string(pm.Protocol),
		}
	}
	startedAt, stoppedAt := v.GetTimes()
	row := VMDetailRow{
		ID:            v.ID,
		Name:          name,
		State:         string(v.GetState()),
		Image:         v.Cfg.ImagePath,
		Memory:        v.Cfg.Memory,
		CPUs:          v.Cfg.CPUs,
		Health:        health,
		RestartPolicy: string(v.Cfg.Restart.Policy),
		RestartCount:  v.GetRestartCount(),
		CreatedAt:     v.CreatedAt.Format(time.RFC3339),
		IPAddress:     v.Cfg.IPAddress,
		GatewayIP:     v.Cfg.GatewayIP,
		Env:           v.Cfg.Env,
		Ports:         ports,
	}
	if startedAt != nil {
		row.StartedAt = startedAt.Format(time.RFC3339)
	}
	if stoppedAt != nil {
		row.StoppedAt = stoppedAt.Format(time.RFC3339)
	}
	return row
}

func Serve(ctx context.Context, addr string, mgr vm.Manager, version string) error {
	h := NewHandler(mgr, addr, version)
	mux := http.NewServeMux()
	mux.Handle("/", h)
	mux.Handle("/ui", h)
	mux.Handle("/ui/", h)
	mux.Handle("/ui/api/vms", h)

	srv := &http.Server{Addr: addr, Handler: mux}
	slog.Info("dashboard server listening", "addr", addr)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("dashboard server shutdown", "err", err)
		}
		return nil
	case err := <-errCh:
		return fmt.Errorf("dashboard server: %w", err)
	}
}

const dashboardTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Uni — Dashboard</title>
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #1a1a2e; color: #eee; padding: 2rem; }
h1 { font-size: 1.8rem; margin-bottom: 1rem; }
h1 span { color: #0f3460; background: #e94560; padding: 0.2rem 0.6rem; border-radius: 4px; font-size: 0.8rem; vertical-align: middle; margin-left: 0.5rem; }
.container { max-width: 1100px; margin: 0 auto; }
table { width: 100%; border-collapse: collapse; background: #16213e; border-radius: 8px; overflow: hidden; }
th, td { padding: 0.75rem 1rem; text-align: left; border-bottom: 1px solid #1a1a2e; }
th { background: #0f3460; font-weight: 600; font-size: 0.85rem; text-transform: uppercase; letter-spacing: 0.05em; }
td { font-size: 0.9rem; }
a { color: #4ecca3; text-decoration: none; }
a:hover { text-decoration: underline; }
.state-running { color: #4ecca3; }
.state-stopped { color: #e94560; }
.state-starting { color: #f0a500; }
.state-stopping { color: #f0a500; }
.state-created { color: #aaa; }
.health-healthy { color: #4ecca3; }
.health-unhealthy { color: #e94560; }
.health-starting { color: #f0a500; }
.health-unknown { color: #888; }
.empty { text-align: center; padding: 3rem; color: #888; }
footer { margin-top: 2rem; text-align: center; color: #555; font-size: 0.8rem; }
</style>
</head>
<body>
<div class="container">
<h1>Uni Dashboard <span>v{{.Version}}</span></h1>

<table>
<thead>
<tr><th>ID</th><th>Name</th><th>State</th><th>Health</th><th>Image</th></tr>
</thead>
<tbody>
{{if .VMs}}
{{range .VMs}}
<tr>
<td><a href="/ui/vm/{{.ID}}">{{.ID}}</a></td>
<td>{{.Name}}</td>
<td class="state-{{.State}}">{{.State}}</td>
<td class="health-{{.Health}}">{{.Health}}</td>
<td>{{.Image}}</td>
</tr>
{{end}}
{{else}}
<tr><td colspan="5" class="empty">No VMs registered</td></tr>
{{end}}
</tbody>
</table>

<footer>Uni — Unikernel Engine &middot; Dashboard</footer>
</div>
</body>
</html>`

const detailTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Uni — VM {{.VM.ID}}</title>
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #1a1a2e; color: #eee; padding: 2rem; }
h1 { font-size: 1.8rem; margin-bottom: 1rem; }
h1 span { color: #0f3460; background: #e94560; padding: 0.2rem 0.6rem; border-radius: 4px; font-size: 0.8rem; vertical-align: middle; margin-left: 0.5rem; }
.container { max-width: 1100px; margin: 0 auto; }
a { color: #4ecca3; text-decoration: none; }
a:hover { text-decoration: underline; }
.back { margin-bottom: 1.5rem; font-size: 0.9rem; }
.card { background: #16213e; border-radius: 8px; padding: 1.5rem; margin-bottom: 1.5rem; }
.card h2 { font-size: 1.1rem; margin-bottom: 1rem; color: #0f3460; background: #1a1a2e; padding: 0.4rem 0.8rem; border-radius: 4px; }
.grid { display: grid; grid-template-columns: 1fr 1fr; gap: 0.75rem; }
.field { display: flex; justify-content: space-between; padding: 0.4rem 0; border-bottom: 1px solid #1a1a2e; }
.field .label { color: #aaa; font-size: 0.85rem; text-transform: uppercase; }
.field .value { font-weight: 500; }
.state-running { color: #4ecca3; }
.state-stopped { color: #e94560; }
.state-starting { color: #f0a500; }
.state-stopping { color: #f0a500; }
.state-created { color: #aaa; }
.health-healthy { color: #4ecca3; }
.health-unhealthy { color: #e94560; }
.health-starting { color: #f0a500; }
.health-unknown { color: #888; }
.log-box { background: #0d0d1a; border-radius: 4px; padding: 1rem; max-height: 400px; overflow-y: auto; font-family: "Courier New", monospace; font-size: 0.8rem; line-height: 1.4; white-space: pre-wrap; word-break: break-all; }
.log-empty { color: #555; font-style: italic; }
footer { margin-top: 2rem; text-align: center; color: #555; font-size: 0.8rem; }
</style>
</head>
<body>
<div class="container">
<h1>VM {{.VM.ID}} <span>v{{.Version}}</span></h1>

<div class="back"><a href="{{.BackLink}}">&larr; Back to Dashboard</a></div>

<div class="card">
<h2>Overview</h2>
<div class="grid">
<div class="field"><span class="label">Name</span><span class="value">{{.VM.Name}}</span></div>
<div class="field"><span class="label">State</span><span class="value state-{{.VM.State}}">{{.VM.State}}</span></div>
<div class="field"><span class="label">Health</span><span class="value health-{{.VM.Health}}">{{.VM.Health}}</span></div>
<div class="field"><span class="label">Image</span><span class="value">{{.VM.Image}}</span></div>
<div class="field"><span class="label">Memory</span><span class="value">{{.VM.Memory}}</span></div>
<div class="field"><span class="label">CPUs</span><span class="value">{{.VM.CPUs}}</span></div>
<div class="field"><span class="label">IP Address</span><span class="value">{{.VM.IPAddress}}</span></div>
<div class="field"><span class="label">Gateway</span><span class="value">{{.VM.GatewayIP}}</span></div>
<div class="field"><span class="label">Restart Policy</span><span class="value">{{.VM.RestartPolicy}}</span></div>
<div class="field"><span class="label">Restart Count</span><span class="value">{{.VM.RestartCount}}</span></div>
<div class="field"><span class="label">Created</span><span class="value">{{.VM.CreatedAt}}</span></div>
<div class="field"><span class="label">Started</span><span class="value">{{.VM.StartedAt}}</span></div>
<div class="field"><span class="label">Stopped</span><span class="value">{{.VM.StoppedAt}}</span></div>
</div>
</div>

{{if .VM.Ports}}
<div class="card">
<h2>Port Mappings</h2>
<table style="width:100%;border-collapse:collapse;">
<tr><th style="text-align:left;padding:0.4rem 0.8rem;border-bottom:1px solid #1a1a2e;">Host</th><th style="text-align:left;padding:0.4rem 0.8rem;border-bottom:1px solid #1a1a2e;">Guest</th><th style="text-align:left;padding:0.4rem 0.8rem;border-bottom:1px solid #1a1a2e;">Protocol</th></tr>
{{range .VM.Ports}}
<tr><td style="padding:0.3rem 0.8rem;">{{.HostPort}}</td><td style="padding:0.3rem 0.8rem;">{{.GuestPort}}</td><td style="padding:0.3rem 0.8rem;">{{.Protocol}}</td></tr>
{{end}}
</table>
</div>
{{end}}

{{if .VM.Env}}
<div class="card">
<h2>Environment Variables</h2>
<div class="log-box">{{range .VM.Env}}{{.}}
{{end}}</div>
</div>
{{end}}

<div class="card">
<h2>Serial Console Output</h2>
{{if .Logs}}
<div class="log-box">{{.Logs}}</div>
{{else}}
<div class="log-box log-empty">No console output captured</div>
{{end}}
</div>

<footer>Uni — Unikernel Engine &middot; Dashboard</footer>
</div>
</body>
</html>`