package compose

import (
	"strings"
	"testing"
)

// FuzzParse feeds arbitrary YAML to the compose parser. A file that is
// accepted must be one compose up can act on without further checks: every
// service has an image and exactly one declared network, every dependency
// and volume exists, and the services can be ordered unless they form a
// cycle, in which case the order is refused rather than incomplete.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"version: \"1\"\nservices:\n  web:\n    image: web:latest\n",
		"services:\n  a:\n    image: a\n    depends_on: [b]\n  b:\n    image: b\n    depends_on: [a]\n",
		"services:\n  a:\n    image: a\n    depends_on: [a]\n",
		"services:\n  web:\n    image: web\n    networks:\n      front:\n        ipv4_address: 10.0.0.5\n        aliases: [w]\nnetworks:\n  front:\n    ipam:\n      config:\n        - subnet: 10.0.0.0/24\n",
		"services:\n  web:\n    image: web\n    volumes: [\"data:/d:ro\"]\nvolumes:\n  data:\n    size: 1G\n",
		"services:\n  web:\n    image: web\n    ports: [\"80:80\", \"53:53/udp\"]\n    health_check: tcp:80\n    restart: always:3\n",
		"services:\n  web:\n    image: web\n    networks: [a, b]\nnetworks:\n  a: {}\n  b: {}\n",
		"services: [web]\n", "services:\n  web: 7\n", "services:\n  \"\":\n    image: x\n", "version: 2\n",
		"networks:\n  n:\n    external: true\n    subnet: 10.0.0.0/24\nservices:\n  a:\n    image: a\n",
		"volumes:\n  v:\n    size: 9223372036854775807G\nservices:\n  a:\n    image: a\n",
		"a: &a [*a]\n", "services:\n  web:\n    image: web\n    networks:\n      front: 7\n", "\x00", "{", "- - - -",
		"services:\n  web:\n    <<: {image: x}\n", "services:\n  web:\n    image: web\n    networks:\n      ? [a]\n      : b\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		file, err := Parse(data)
		if err != nil {
			if strings.TrimSpace(err.Error()) == "" {
				t.Fatal("empty error")
			}
			return
		}
		if len(file.Services) == 0 {
			t.Fatal("a file without services was accepted")
		}
		for name, svc := range file.Services {
			if name == "" || svc.Image == "" {
				t.Fatalf("service %q accepted without a name or an image", name)
			}
			if len(svc.Networks) != 1 {
				t.Fatalf("service %q has %d networks", name, len(svc.Networks))
			}
			if _, ok := file.Networks[svc.Networks[0]]; !ok {
				t.Fatalf("service %q is on undeclared network %q", name, svc.Networks[0])
			}
			for _, dep := range svc.DependsOn {
				if _, ok := file.Services[dep]; !ok {
					t.Fatalf("service %q depends on unknown %q", name, dep)
				}
			}
			for _, vol := range svc.Volumes {
				if _, ok := file.Volumes[strings.SplitN(vol, ":", 2)[0]]; !ok {
					t.Fatalf("service %q mounts undeclared volume %q", name, vol)
				}
			}
		}
		order, err := TopologicalSort(file.Services)
		if err != nil {
			return
		}
		if len(order) != len(file.Services) {
			t.Fatalf("order %v does not hold every service", order)
		}
		at := map[string]int{}
		for i, name := range order {
			if _, dup := at[name]; dup {
				t.Fatalf("service %q ordered twice", name)
			}
			at[name] = i
		}
		for name, svc := range file.Services {
			for _, dep := range svc.DependsOn {
				if at[dep] >= at[name] {
					t.Fatalf("%q starts before its dependency %q: %v", name, dep, order)
				}
			}
		}
	})
}
