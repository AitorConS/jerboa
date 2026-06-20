package compose

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/AitorConS/unikernel-engine/internal/volume"
	"gopkg.in/yaml.v3"
)

// Parse decodes a compose YAML document and validates it.
func Parse(data []byte) (File, error) {
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return File{}, fmt.Errorf("compose parse: %w", err)
	}
	if err := validate(f); err != nil {
		return File{}, err
	}
	return f, nil
}

func validate(f File) error {
	if f.Version != "" && f.Version != "1" {
		return fmt.Errorf("compose: unsupported version %q (expected \"1\")", f.Version)
	}
	if len(f.Services) == 0 {
		return fmt.Errorf("compose: at least one service required")
	}
	for name, svc := range f.Services {
		if name == "" {
			return fmt.Errorf("compose: service name must not be empty")
		}
		if svc.Image == "" {
			return fmt.Errorf("compose: service %q missing image", name)
		}
		for _, dep := range svc.DependsOn {
			if _, ok := f.Services[dep]; !ok {
				return fmt.Errorf("compose: service %q depends_on unknown service %q", name, dep)
			}
		}
		for _, net := range svc.Networks {
			if _, ok := f.Networks[net]; !ok {
				return fmt.Errorf("compose: service %q references unknown network %q", name, net)
			}
		}
		for _, port := range svc.Ports {
			if err := validatePortSpec(port); err != nil {
				return fmt.Errorf("compose: service %q ports: %w", name, err)
			}
		}
		for _, vol := range svc.Volumes {
			if err := validateVolumeSpec(vol); err != nil {
				return fmt.Errorf("compose: service %q volumes: %w", name, err)
			}
			volName := strings.SplitN(vol, ":", 2)[0]
			if len(f.Volumes) > 0 {
				if _, ok := f.Volumes[volName]; !ok {
					return fmt.Errorf("compose: service %q references unknown volume %q", name, volName)
				}
			}
		}
		if svc.HealthCheck != "" {
			if err := validateHealthCheckSpec(svc.HealthCheck); err != nil {
				return fmt.Errorf("compose: service %q health_check: %w", name, err)
			}
		}
		if svc.Restart != "" {
			if err := validateRestartSpec(svc.Restart); err != nil {
				return fmt.Errorf("compose: service %q restart: %w", name, err)
			}
		}
		if svc.Replicas < 0 {
			return fmt.Errorf("compose: service %q replicas must be non-negative, got %d", name, svc.Replicas)
		}
		if svc.Strategy != "" && svc.Strategy != "RollingUpdate" && svc.Strategy != "Recreate" {
			return fmt.Errorf("compose: service %q strategy must be RollingUpdate or Recreate, got %q", name, svc.Strategy)
		}
	}
	for name, vc := range f.Volumes {
		if vc.Size != "" {
			if _, err := volume.ParseSize(vc.Size); err != nil {
				return fmt.Errorf("compose: volume %q: invalid size %q: %w", name, vc.Size, err)
			}
		}
	}
	return nil
}

// validatePortSpec checks that s is a valid host:guest[/proto] spec.
func validatePortSpec(s string) error {
	// Strip optional protocol suffix.
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		proto := strings.ToLower(s[idx+1:])
		if proto != "tcp" && proto != "udp" {
			return fmt.Errorf("port %q: unknown protocol %q", s, proto)
		}
		s = s[:idx]
	}
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("port %q: expected host:guest format", s)
	}
	for _, p := range parts {
		if p == "" {
			return fmt.Errorf("port %q: port number must not be empty", s)
		}
	}
	return nil
}

// validateVolumeSpec checks that s is a valid name:guestpath[:ro] spec.
func validateVolumeSpec(s string) error {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return fmt.Errorf("volume %q: expected name:guestpath[:ro]", s)
	}
	if parts[0] == "" {
		return fmt.Errorf("volume %q: name must not be empty", s)
	}
	if parts[1] == "" {
		return fmt.Errorf("volume %q: guest path must not be empty", s)
	}
	if len(parts) == 3 && !strings.EqualFold(parts[2], "ro") {
		return fmt.Errorf("volume %q: third field must be \"ro\" if present", s)
	}
	return nil
}

func validateHealthCheckSpec(s string) error {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) < 2 {
		return fmt.Errorf("expected tcp:PORT or http:PORT:/path")
	}
	hcType := strings.ToLower(parts[0])
	if hcType != "tcp" && hcType != "http" {
		return fmt.Errorf("type must be tcp or http, got %q", hcType)
	}
	rest := parts[1]
	if hcType == "http" {
		slashIdx := strings.Index(rest, "/")
		if slashIdx >= 0 {
			rest = rest[:slashIdx]
		}
	}
	if _, err := strconv.Atoi(rest); err != nil {
		return fmt.Errorf("port must be a number")
	}
	return nil
}

func validateRestartSpec(s string) error {
	parts := strings.SplitN(s, ":", 2)
	policy := strings.ToLower(parts[0])
	if policy != "never" && policy != "on-failure" && policy != "always" {
		return fmt.Errorf("must be never, on-failure, or always, got %q", policy)
	}
	if len(parts) == 2 {
		if _, err := strconv.Atoi(parts[1]); err != nil {
			return fmt.Errorf("max-retries must be a number")
		}
	}
	return nil
}
