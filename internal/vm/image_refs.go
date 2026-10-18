package vm

import (
	"path/filepath"
	"strings"
)

// ImageDigestsInUse returns the image store digests ("sha256:<hex>") that the
// given VMs boot from. A VM keeps the store image it was created from: every
// start clones its private boot disk from Cfg.ImagePath, so that image must
// survive even after its tag moves to a newer build.
func ImageDigestsInUse(vms []*VM) map[string]bool {
	inUse := make(map[string]bool)
	add := func(d string) {
		if hex, ok := strings.CutPrefix(d, "sha256:"); ok && len(hex) == 64 {
			inUse[d] = true
		}
	}
	for _, v := range vms {
		if v == nil {
			continue
		}
		add(v.Cfg.ImageDigest)
		add(v.Cfg.ImageRef)
		if v.Cfg.ImagePath != "" {
			// Store layout: <root>/<hex>/disk.img.
			add("sha256:" + filepath.Base(filepath.Dir(v.Cfg.ImagePath)))
		}
	}
	return inUse
}
