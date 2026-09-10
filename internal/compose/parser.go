package compose

import (
	"fmt"
	"strings"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/volume"
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

func validatePortSpec(s string) error   { _, err := api.ParsePortMap(s); return err }
func validateVolumeSpec(s string) error { _, _, _, err := api.ParseVolumeMount(s); return err }

func validateHealthCheckSpec(s string) error { _, err := api.ParseHealthCheck(s); return err }
func validateRestartSpec(s string) error     { _, err := api.ParseRestartPolicy(s); return err }
