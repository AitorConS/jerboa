package compose_test

import (
	"testing"

	"github.com/AitorConS/unikernel-engine/internal/compose"
	"github.com/stretchr/testify/require"
)

var validYAML = []byte(`
version: "1"
services:
  backend:
    image: api:latest
    memory: 512M
    cpus: 2
    networks:
      - app-net
  frontend:
    image: web:latest
    memory: 256M
    depends_on:
      - backend
    networks:
      - app-net
networks:
  app-net:
    driver: bridge
`)

func TestParse_Valid(t *testing.T) {
	f, err := compose.Parse(validYAML)
	require.NoError(t, err)
	require.Equal(t, "1", f.Version)
	require.Len(t, f.Services, 2)
	require.Equal(t, "api:latest", f.Services["backend"].Image)
	require.Equal(t, []string{"backend"}, f.Services["frontend"].DependsOn)
}

func TestParse_MissingVersion(t *testing.T) {
	f, err := compose.Parse([]byte(`services: {a: {image: x}}`))
	require.NoError(t, err)
	require.Len(t, f.Services, 1)
}

func TestParse_UnsupportedVersion(t *testing.T) {
	_, err := compose.Parse([]byte(`version: "2"\nservices: {a: {image: x}}`))
	require.Error(t, err)
}

func TestParse_NoServices(t *testing.T) {
	_, err := compose.Parse([]byte(`version: "1"`))
	require.ErrorContains(t, err, "at least one service")
}

func TestParse_MissingImage(t *testing.T) {
	_, err := compose.Parse([]byte(`
version: "1"
services:
  broken:
    memory: 256M
`))
	require.ErrorContains(t, err, "missing image")
}

func TestParse_UnknownDependency(t *testing.T) {
	_, err := compose.Parse([]byte(`
version: "1"
services:
  a:
    image: x
    depends_on: [nonexistent]
`))
	require.ErrorContains(t, err, "unknown service")
}

func TestParse_UnknownNetwork(t *testing.T) {
	_, err := compose.Parse([]byte(`
version: "1"
services:
  a:
    image: x
    networks: [no-such-net]
`))
	require.ErrorContains(t, err, "unknown network")
}

func TestParse_InvalidYAML(t *testing.T) {
	_, err := compose.Parse([]byte(`:::invalid yaml:::`))
	require.Error(t, err)
}

func TestTopologicalSort_NoDeps(t *testing.T) {
	services := map[string]compose.Service{
		"a": {Image: "a:latest"},
		"b": {Image: "b:latest"},
		"c": {Image: "c:latest"},
	}
	order, err := compose.TopologicalSort(services)
	require.NoError(t, err)
	require.Len(t, order, 3)
}

func TestTopologicalSort_Chain(t *testing.T) {
	services := map[string]compose.Service{
		"db":  {Image: "db:latest"},
		"api": {Image: "api:latest", DependsOn: []string{"db"}},
		"web": {Image: "web:latest", DependsOn: []string{"api"}},
	}
	order, err := compose.TopologicalSort(services)
	require.NoError(t, err)
	require.Equal(t, []string{"db", "api", "web"}, order)
}

func TestTopologicalSort_DiamondDeps(t *testing.T) {
	services := map[string]compose.Service{
		"base":  {Image: "base:latest"},
		"left":  {Image: "left:latest", DependsOn: []string{"base"}},
		"right": {Image: "right:latest", DependsOn: []string{"base"}},
		"top":   {Image: "top:latest", DependsOn: []string{"left", "right"}},
	}
	order, err := compose.TopologicalSort(services)
	require.NoError(t, err)
	require.Equal(t, "base", order[0])
	require.Equal(t, "top", order[len(order)-1])
}

func TestTopologicalSort_Cycle(t *testing.T) {
	services := map[string]compose.Service{
		"a": {Image: "a:latest", DependsOn: []string{"b"}},
		"b": {Image: "b:latest", DependsOn: []string{"a"}},
	}
	_, err := compose.TopologicalSort(services)
	require.ErrorContains(t, err, "cycle")
}

func TestTopologicalSort_SelfDep(t *testing.T) {
	services := map[string]compose.Service{
		"a": {Image: "a:latest", DependsOn: []string{"a"}},
	}
	_, err := compose.TopologicalSort(services)
	require.ErrorContains(t, err, "cycle")
}

// --- port validation tests ---

func TestParse_ValidPorts(t *testing.T) {
	data := []byte(`
version: "1"
services:
  web:
    image: nginx:latest
    ports:
      - "8080:80"
      - "443:443/tcp"
      - "5353:53/udp"
`)
	f, err := compose.Parse(data)
	require.NoError(t, err)
	require.Equal(t, []string{"8080:80", "443:443/tcp", "5353:53/udp"}, f.Services["web"].Ports)
}

func TestParse_InvalidPortProto(t *testing.T) {
	_, err := compose.Parse([]byte(`
version: "1"
services:
  a:
    image: x
    ports:
      - "80:80/sctp"
`))
	require.ErrorContains(t, err, "unknown protocol")
}

func TestParse_InvalidPortFormat(t *testing.T) {
	_, err := compose.Parse([]byte(`
version: "1"
services:
  a:
    image: x
    ports:
      - "8080"
`))
	require.ErrorContains(t, err, "host:guest")
}

// --- volume validation tests ---

func TestParse_ValidVolumesWithTopLevel(t *testing.T) {
	data := []byte(`
version: "1"
services:
  db:
    image: redis:latest
    volumes:
      - "data:/data"
      - "backup:/backup:ro"
volumes:
  data:
    size: 512M
  backup:
`)
	f, err := compose.Parse(data)
	require.NoError(t, err)
	require.Equal(t, []string{"data:/data", "backup:/backup:ro"}, f.Services["db"].Volumes)
	require.Equal(t, "512M", f.Volumes["data"].Size)
	require.Equal(t, "", f.Volumes["backup"].Size)
}

func TestParse_VolumeDefaultSize(t *testing.T) {
	vc := compose.VolumeConfig{}
	require.Equal(t, "1G", vc.DefaultSize())
	vc = compose.VolumeConfig{Size: "512M"}
	require.Equal(t, "512M", vc.DefaultSize())
}

func TestParse_VolumesWithoutTopLevelAllowed(t *testing.T) {
	data := []byte(`
version: "1"
services:
  db:
    image: redis:latest
    volumes:
      - "data:/data"
`)
	f, err := compose.Parse(data)
	require.NoError(t, err)
	require.Len(t, f.Volumes, 0)
}

func TestParse_UnknownVolume(t *testing.T) {
	data := []byte(`
version: "1"
services:
  db:
    image: redis:latest
    volumes:
      - "nonexistent:/data"
volumes:
  data:
    size: 512M
`)
	_, err := compose.Parse(data)
	require.ErrorContains(t, err, "unknown volume")
}

func TestParse_InvalidVolumeSize(t *testing.T) {
	data := []byte(`
version: "1"
services:
  db:
    image: redis:latest
    volumes:
      - "data:/data"
volumes:
  data:
    size: badsize
`)
	_, err := compose.Parse(data)
	require.ErrorContains(t, err, "invalid size")
}

func TestParse_InvalidVolumeSpec(t *testing.T) {
	data := []byte(`
version: "1"
services:
  db:
    image: redis:latest
    volumes:
      - "nocolon"
volumes:
  nocolon:
`)
	_, err := compose.Parse(data)
	require.ErrorContains(t, err, "name:guestpath")
}

func TestParse_InvalidVolumeThirdField(t *testing.T) {
	data := []byte(`
version: "1"
services:
  db:
    image: redis:latest
    volumes:
      - "data:/data:rw"
volumes:
  data:
`)
	_, err := compose.Parse(data)
	require.ErrorContains(t, err, "ro")
}

func TestParse_EmptyVolumeName(t *testing.T) {
	data := []byte(`
version: "1"
services:
  db:
    image: redis:latest
    volumes:
      - ":/data"
volumes:
  "":
`)
	_, err := compose.Parse(data)
	require.Error(t, err)
}

func TestParse_NoVolumesTopLevelIsOK(t *testing.T) {
	data := []byte(`
version: "1"
services:
  web:
    image: nginx:latest
`)
	f, err := compose.Parse(data)
	require.NoError(t, err)
	require.Len(t, f.Volumes, 0)
}

func TestParse_HealthCheckTCP(t *testing.T) {
	data := []byte(`
version: "1"
services:
  api:
    image: api:latest
    health_check: "tcp:8080"
`)
	f, err := compose.Parse(data)
	require.NoError(t, err)
	require.Equal(t, "tcp:8080", f.Services["api"].HealthCheck)
}

func TestParse_HealthCheckHTTP(t *testing.T) {
	data := []byte(`
version: "1"
services:
  web:
    image: web:latest
    health_check: "http:80/health"
`)
	f, err := compose.Parse(data)
	require.NoError(t, err)
	require.Equal(t, "http:80/health", f.Services["web"].HealthCheck)
}

func TestParse_HealthCheckInvalid(t *testing.T) {
	_, err := compose.Parse([]byte(`
version: "1"
services:
  api:
    image: api:latest
    health_check: "udp:80"
`))
	require.ErrorContains(t, err, "health_check")
}

func TestParse_HealthCheckBadPort(t *testing.T) {
	_, err := compose.Parse([]byte(`
version: "1"
services:
  api:
    image: api:latest
    health_check: "tcp:abc"
`))
	require.ErrorContains(t, err, "health_check")
}

func TestParse_HealthCheckMissingPort(t *testing.T) {
	_, err := compose.Parse([]byte(`
version: "1"
services:
  api:
    image: api:latest
    health_check: "tcp"
`))
	require.ErrorContains(t, err, "health_check")
}

func TestParse_RestartNever(t *testing.T) {
	data := []byte(`
version: "1"
services:
  api:
    image: api:latest
    restart: never
`)
	f, err := compose.Parse(data)
	require.NoError(t, err)
	require.Equal(t, "never", f.Services["api"].Restart)
}

func TestParse_RestartAlwaysWithRetries(t *testing.T) {
	data := []byte(`
version: "1"
services:
  api:
    image: api:latest
    restart: "always:5"
`)
	f, err := compose.Parse(data)
	require.NoError(t, err)
	require.Equal(t, "always:5", f.Services["api"].Restart)
}

func TestParse_RestartInvalid(t *testing.T) {
	_, err := compose.Parse([]byte(`
version: "1"
services:
  api:
    image: api:latest
    restart: always
    health_check: "tcp:bad"
`))
	require.Error(t, err)
}

func TestParse_RestartBadPolicy(t *testing.T) {
	_, err := compose.Parse([]byte(`
version: "1"
services:
  api:
    image: api:latest
    restart: invalid
`))
	require.ErrorContains(t, err, "restart")
}

func TestParse_RestartBadRetries(t *testing.T) {
	_, err := compose.Parse([]byte(`
version: "1"
services:
  api:
    image: api:latest
    restart: "on-failure:abc"
`))
	require.ErrorContains(t, err, "restart")
}
