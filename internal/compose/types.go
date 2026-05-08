package compose

// File is a parsed uni compose file.
type File struct {
	// Version must be "1".
	Version string `yaml:"version"`
	// Services maps service name to its definition.
	Services map[string]Service `yaml:"services"`
	// Networks maps network name to its definition.
	Networks map[string]Network `yaml:"networks"`
	// Volumes maps volume name to its configuration.
	// Top-level volumes are created on "compose up" and optionally
	// removed on "compose down --volumes".
	Volumes map[string]VolumeConfig `yaml:"volumes"`
}

// Service describes a single unikernel service.
type Service struct {
	Image       string   `yaml:"image"`
	Memory      string   `yaml:"memory"`
	CPUs        int      `yaml:"cpus"`
	DependsOn   []string `yaml:"depends_on"`
	Networks    []string `yaml:"networks"`
	Environment []string `yaml:"environment"`
	Ports       []string `yaml:"ports"`
	Volumes     []string `yaml:"volumes"`
	HealthCheck string   `yaml:"health_check,omitempty"`
	Restart     string   `yaml:"restart,omitempty"`
}

// Network describes a logical network.
type Network struct {
	Driver string `yaml:"driver"`
	Subnet string `yaml:"subnet,omitempty"`
}

// VolumeConfig describes a named volume defined at the top level of a compose file.
type VolumeConfig struct {
	// Size is the volume size as a human-readable string (e.g. "512M", "1G").
	// Defaults to "1G" if empty.
	Size string `yaml:"size"`
}

// DefaultSize returns the volume size, falling back to "1G".
func (vc VolumeConfig) DefaultSize() string {
	if vc.Size == "" {
		return "1G"
	}
	return vc.Size
}

// State records running VM IDs and created volumes for a compose project.
type State struct {
	Project         string            `json:"project"`
	Services        map[string]string `json:"services"`
	ServiceNetworks map[string]string `json:"service_networks,omitempty"`
	ServiceIPs      map[string]string `json:"service_ips,omitempty"`
	CreatedVolumes  []string          `json:"created_volumes,omitempty"`
	CreatedNetworks []string          `json:"created_networks,omitempty"`
}
