//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type composeRow struct {
	Service string `json:"service"`
	ID      string `json:"id"`
	State   string `json:"state"`
}

// stack is a compose project of two probe guests: db keeps a volume, web
// depends on it and reaches it by name.
type stack struct {
	e    *env
	file string
	db   *guest
	web  *guest
}

// composeSubnet returns a subnet no other network of the run uses.
func composeSubnet() string {
	n := suite.seq.Add(1)
	return fmt.Sprintf("10.%d.%d.0/24", 209+int(n/250), n%250)
}

func newStack(e *env, webImage string) *stack {
	e.t.Helper()
	requireGuestNetworking(e.t)
	s := &stack{e: e, file: filepath.Join(e.root, "stack", "compose.yaml")}
	s.db = &guest{e: e, name: "db", port: freePort(e.t)}
	s.web = &guest{e: e, name: "web", port: freePort(e.t)}
	yaml := fmt.Sprintf(`version: "1"
services:
  web:
    image: %s
    depends_on: [db]
    networks: [app]
    environment:
      - PROBE_MARKER=web
    ports:
      - "127.0.0.1:%d:8080"
  db:
    image: probe
    networks: [app]
    environment:
      - PROBE_MARKER=db
    ports:
      - "127.0.0.1:%d:8080"
    volumes:
      - data:/data
networks:
  app:
    driver: bridge
    subnet: %s
volumes:
  data:
    size: 64M
`, webImage, s.web.port, s.db.port, composeSubnet())
	require.NoError(e.t, os.MkdirAll(filepath.Dir(s.file), 0o755))
	require.NoError(e.t, os.WriteFile(s.file, []byte(yaml), 0o600))
	return s
}

// stateFiles returns the compose state files in the user's ~/.jerboa/compose.
func (s *stack) stateFiles() []string {
	s.e.t.Helper()
	files, err := filepath.Glob(filepath.Join(s.e.home, ".jerboa", "compose", "*.json"))
	require.NoError(s.e.t, err)
	return files
}

func (s *stack) ps() map[string]composeRow {
	s.e.t.Helper()
	var rows []composeRow
	s.e.jsonOut(&rows, "compose", "ps", s.file)
	out := map[string]composeRow{}
	for _, r := range rows {
		out[r.Service] = r
	}
	return out
}

// up brings the stack up and records the VM of each service.
func (s *stack) up() {
	s.e.t.Helper()
	s.e.ok("compose", "up", s.file)
	rows := s.ps()
	require.Len(s.e.t, rows, 2)
	s.db.id, s.web.id = rows["db"].ID, rows["web"].ID
	s.e.vmIDs[s.db.id], s.e.vmIDs[s.web.id] = true, true
	s.db.waitReady()
	s.web.waitReady()
}

type volumeRow struct {
	ID string `json:"id"`
}

func (e *env) volumes() []string {
	e.t.Helper()
	var rows []volumeRow
	e.jsonOut(&rows, "volume", "ls")
	var out []string
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func (e *env) networks() []string {
	e.t.Helper()
	var rows []struct {
		Name string `json:"name"`
	}
	e.jsonOut(&rows, "network", "ls")
	var out []string
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}

// TestComposeLifecycle brings a two-service stack up and down the way a user
// does and checks each thing compose promises: services start in dependency
// order and find each other by name, the state lives in ~/.jerboa/compose
// and not in the project, a second up is refused, down removes the VMs and
// the network but keeps the volume and its data, and down --volumes removes
// the volume too.
func TestComposeLifecycle(t *testing.T) {
	e := newEnv(t)
	e.buildProbe()
	s := newStack(e, "probe")

	s.up()
	require.Len(t, s.stateFiles(), 1, "compose up must record the stack in ~/.jerboa/compose")
	entries, err := os.ReadDir(filepath.Dir(s.file))
	require.NoError(t, err)
	require.Len(t, entries, 1, "compose up must not write into the project directory")

	rows := s.ps()
	require.Equal(t, "running", rows["db"].State)
	require.Equal(t, "running", rows["web"].State)
	require.Equal(t, "db", s.db.status().Marker)
	require.Equal(t, "web", s.web.status().Marker)

	// web reaches db by its service name, over the stack's network.
	dbIP := e.inspect(s.db.id).IPAddress
	require.NotEmpty(t, dbIP)
	require.Equal(t, []string{dbIP}, s.web.resolve("db").Addrs)
	body := s.web.get("/fetch?url=" + url.QueryEscape("http://db:8080/"))
	require.Contains(t, body, `"marker":"db"`)
	s.db.write("note", "first deployment")

	// The stack owns a network and a volume, under names scoped to it.
	require.Len(t, e.networks(), 1)
	vols := e.volumes()
	require.Len(t, vols, 1)
	require.NotEqual(t, "data", vols[0], "a stack's volume must be scoped to the stack")
	require.Contains(t, vols[0], "data")
	require.Contains(t, e.ok("compose", "logs", s.file, "db"), "PROBE_READY marker=db")

	// A second up must not start a second copy.
	out := e.fails("compose", "up", s.file)
	require.Contains(t, out, "already has state")
	require.Len(t, e.ps(), 2)

	out = e.ok("compose", "down", s.file)
	require.Contains(t, out, "stopped db")
	require.Contains(t, out, "stopped web")
	require.Empty(t, e.ps(), "compose down must remove the service VMs")
	require.Empty(t, e.networks(), "compose down must remove the network it created")
	require.Equal(t, vols, e.volumes(), "compose down without --volumes must keep the volume")
	require.Empty(t, s.stateFiles(), "compose down must remove the state file")
	e.waitNoVMM(s.db.id, 15*time.Second)
	e.waitNoVMM(s.web.id, 15*time.Second)
	// Down again is a no-op, not an error.
	e.ok("compose", "down", s.file)

	// Up again: new VMs, the same volume with what the first ones wrote.
	firstDB := s.db.id
	s.up()
	require.NotEqual(t, firstDB, s.db.id)
	require.Equal(t, "first deployment", s.db.read("note"))
	require.Equal(t, 2, s.db.status().Boots)

	e.ok("compose", "down", s.file, "--volumes")
	require.Empty(t, e.ps())
	require.Empty(t, e.volumes(), "compose down --volumes must remove the stack's volume")
	require.Empty(t, e.networks())
	require.Empty(t, s.stateFiles())
	e.requireHealthy()
}

// TestComposeSurvivesDaemonCrash kills the daemon under a running stack.
// The state file is the client's, so with a new daemon `compose ps` must
// still describe the stack and `compose down` must take all of it down.
func TestComposeSurvivesDaemonCrash(t *testing.T) {
	e := newEnv(t)
	e.buildProbe()
	s := newStack(e, "probe")
	s.up()
	s.db.write("note", "before the crash")

	e.killDaemon()
	e.startDaemon()

	rows := s.ps()
	require.Len(t, rows, 2)
	for _, name := range []string{"db", "web"} {
		require.Containsf(t, []string{"running", "stopped"}, rows[name].State, "service %s after the daemon crash", name)
	}
	requireNoOrphans(e, s.db.id, s.web.id)

	e.ok("compose", "down", s.file)
	require.Empty(t, e.ps())
	require.Empty(t, e.networks())
	require.Empty(t, s.stateFiles())
	e.waitNoVMM(s.db.id, 15*time.Second)
	e.waitNoVMM(s.web.id, 15*time.Second)

	s.up()
	require.Equal(t, "before the crash", s.db.read("note"))
	e.ok("compose", "down", s.file, "--volumes")
	e.requireHealthy()
}

// TestComposeUpFailureCanBeCleaned starts a stack whose second service has
// no image. up must fail, and down must then remove everything the failed
// up created: the first service's VM, the network and the volume.
func TestComposeUpFailureCanBeCleaned(t *testing.T) {
	e := newEnv(t)
	e.buildProbe()
	s := newStack(e, "no-such-image:latest")

	out := e.fails("compose", "up", s.file)
	require.Contains(t, out, "web", "the error must name the service that failed")
	// db started before web failed (depends_on), and is recorded.
	require.Len(t, s.stateFiles(), 1, "a failed up must leave the state down needs")
	vms := e.ps()
	require.Len(t, vms, 1)
	require.Equal(t, "db", vms[0].Name)
	e.vmIDs[vms[0].ID] = true
	// Booted, so that the stop below is the ordinary one and not the stop
	// of a guest that is still starting.
	s.db.id = vms[0].ID
	s.db.waitReady()

	e.ok("compose", "down", s.file, "--volumes")
	require.Empty(t, e.ps())
	require.Empty(t, e.networks())
	require.Empty(t, e.volumes())
	require.Empty(t, s.stateFiles())
	e.waitNoVMM(vms[0].ID, 15*time.Second)
}

// TestComposeRejectsForeignState plants what a hostile project would: a
// state file in ~/.jerboa/compose form that names a VM compose never
// started. compose down must refuse to stop it.
func TestComposeRejectsForeignState(t *testing.T) {
	e := newEnv(t)
	e.buildProbe()
	netName, _ := e.network()
	victim := e.startGuest(guestOpts{name: "victim", network: netName})
	s := newStack(e, "probe")
	s.up()

	files := s.stateFiles()
	require.Len(t, files, 1)
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	var state map[string]any
	require.NoError(t, json.Unmarshal(data, &state))
	services, ok := state["services"].(map[string]any)
	require.True(t, ok, "state file has no services: %s", data)
	services["db"] = victim.id
	data, err = json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(files[0], data, 0o600))

	res := e.run("compose", "down", s.file)
	require.Equalf(t, 1, res.code, "compose down on a state file that names another VM:\n%s", res.all())
	require.Equal(t, "running", e.vmState(victim.id), "compose down stopped a VM that is not the stack's")
	require.Equal(t, "running", e.vmState(s.web.id), "compose down must check every VM before it stops any")
	require.True(t, strings.Contains(res.all(), "victim") || strings.Contains(res.all(), victim.id), "the error must name the VM:\n%s", res.all())
}
