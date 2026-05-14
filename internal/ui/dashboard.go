package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/vm"
)

// DashboardData is the data passed to the dashboard template.
type DashboardData struct {
	VMs     []VMRow
	Total   int
	UIAddr  string
	Version string
}

// VMRow is a single VM row in the dashboard.
type VMRow struct {
	ID        string
	Name      string
	State     string
	Image     string
	Health    string
	CPURender string
	MemRender string
}

// Handler serves the web dashboard.
type Handler struct {
	mgr     vm.Manager
	uiAddr  string
	tmpl    *template.Template
	version string
}

// NewHandler creates a dashboard handler.
func NewHandler(mgr vm.Manager, uiAddr, version string) *Handler {
	tmpl := template.Must(template.New("dashboard").Parse(dashboardTmpl))
	return &Handler{mgr: mgr, uiAddr: uiAddr, tmpl: tmpl, version: version}
}

// ServeHTTP handles dashboard requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/ui" || r.URL.Path == "/ui/" || r.URL.Path == "/" {
		h.serveDashboard(w, r)
		return
	}
	if r.URL.Path == "/ui/api/vms" {
		h.serveVMsJSON(w, r)
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
	if err := h.tmpl.Execute(w, data); err != nil {
		slog.Warn("dashboard template error", "err", err)
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

// Serve starts the dashboard HTTP server.
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
.summary { display: flex; gap: 1rem; margin-bottom: 2rem; flex-wrap: wrap; }
.summary .card { background: #16213e; border-radius: 8px; padding: 1rem 1.5rem; min-width: 120px; }
.summary .card .num { font-size: 2rem; font-weight: bold; }
.summary .card .label { font-size: 0.8rem; color: #aaa; text-transform: uppercase; }
.running .num { color: #4ecca3; }
.stopped .num { color: #e94560; }
.total .num { color: #0f3460; }
table { width: 100%; border-collapse: collapse; background: #16213e; border-radius: 8px; overflow: hidden; }
th, td { padding: 0.75rem 1rem; text-align: left; border-bottom: 1px solid #1a1a2e; }
th { background: #0f3460; font-weight: 600; font-size: 0.85rem; text-transform: uppercase; letter-spacing: 0.05em; }
td { font-size: 0.9rem; }
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

<div class="summary">
{{range .VMs}}{{end}}
</div>

<table>
<thead>
<tr><th>ID</th><th>Name</th><th>State</th><th>Health</th><th>Image</th></tr>
</thead>
<tbody>
{{if .VMs}}
{{range .VMs}}
<tr>
<td>{{.ID}}</td>
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
