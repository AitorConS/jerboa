package pkg

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// PinDocker selects a platform then returns an immutable local image identity.
func PinDocker(ctx context.Context, reference, platform string) (*Provenance, error) {
	if err := ValidatePlatform(platform); err != nil {
		return nil, err
	}
	inspect := func() ([]byte, error) {
		return exec.CommandContext(ctx, "docker", "image", "inspect", reference).Output()
	}
	data, err := inspect()
	if err != nil {
		if out, pullErr := exec.CommandContext(ctx, "docker", "pull", "--platform", platform, reference).CombinedOutput(); pullErr != nil {
			return nil, fmt.Errorf("docker pull: %w: %s", pullErr, out)
		}
		data, err = inspect()
		if err != nil {
			return nil, fmt.Errorf("docker inspect: %w", err)
		}
	}
	var items []struct {
		ID           string `json:"Id"`
		OS           string `json:"Os"`
		Architecture string
		RepoDigests  []string
	}
	if err = json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("invalid Docker inspect response: %w", err)
	}
	if len(items) != 1 {
		return nil, fmt.Errorf("docker inspect returned %d images, expected one", len(items))
	}
	item := items[0]
	if item.OS+"/"+item.Architecture != platform {
		return nil, fmt.Errorf("docker image is %s/%s, requested %s", item.OS, item.Architecture, platform)
	}
	if !strings.HasPrefix(item.ID, "sha256:") {
		return nil, fmt.Errorf("docker image has no immutable ID")
	}
	p := &Provenance{Kind: "docker", Reference: reference, ImageID: item.ID}
	if len(item.RepoDigests) > 0 {
		p.Digest = item.RepoDigests[0]
	}
	return p, nil
}
