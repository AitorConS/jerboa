package api

import (
	"fmt"
	"strconv"
	"strings"
)

func ParseHealthCheck(spec string) (HealthCheckSpec, error) {
	parts := strings.SplitN(spec, ":", 3)
	if len(parts) < 2 {
		return HealthCheckSpec{}, fmt.Errorf("health check format: tcp:PORT or http:PORT:/path")
	}
	hcType := strings.ToLower(parts[0])
	port, err := strconv.Atoi(parts[1])
	if err != nil {
		return HealthCheckSpec{}, fmt.Errorf("health check port must be a number: %w", err)
	}
	if port < 1 || port > 65535 {
		return HealthCheckSpec{}, fmt.Errorf("health check port must be 1-65535")
	}
	if hcType != "tcp" && hcType != "http" {
		return HealthCheckSpec{}, fmt.Errorf("health check type must be tcp or http, got %q", hcType)
	}
	hc := HealthCheckSpec{
		Type: hcType,
		Port: port,
	}
	if hcType == "http" && len(parts) == 3 {
		hc.Path = parts[2]
		if !strings.HasPrefix(hc.Path, "/") {
			hc.Path = "/" + hc.Path
		}
	}
	return hc, nil
}

func ParseRestartPolicy(spec string) (RestartSpec, error) {
	parts := strings.SplitN(spec, ":", 2)
	policy := strings.ToLower(parts[0])
	if policy != "never" && policy != "on-failure" && policy != "always" {
		return RestartSpec{}, fmt.Errorf("restart policy must be never, on-failure, or always, got %q", policy)
	}
	rs := RestartSpec{Policy: policy}
	if len(parts) == 2 {
		maxRetries, err := strconv.Atoi(parts[1])
		if err != nil {
			return RestartSpec{}, fmt.Errorf("restart max-retries must be a number: %w", err)
		}
		if maxRetries < 0 {
			return RestartSpec{}, fmt.Errorf("restart max-retries must be >= 0, got %d", maxRetries)
		}
		rs.MaxRetries = maxRetries
	}
	return rs, nil
}

// ParseVolumeMount splits a named volume mount, validating its guest path and mode.
func ParseVolumeMount(s string) (name, guest string, readOnly bool, err error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || !strings.HasPrefix(parts[1], "/") {
		return "", "", false, fmt.Errorf("volume %q: expected name:/guestpath[:ro]", s)
	}
	if len(parts) == 3 && !strings.EqualFold(parts[2], "ro") {
		return "", "", false, fmt.Errorf("volume %q: mode must be ro", s)
	}
	return parts[0], parts[1], len(parts) == 3, nil
}
