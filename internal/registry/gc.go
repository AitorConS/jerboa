package registry

import (
	"fmt"

	"github.com/AitorConS/unikernel-engine/internal/ociblob"
)

// GCResult contains summary information for a GC run.
type GCResult struct {
	Removed int
	Kept    int
}

// GarbageCollect deletes OCI blobs not referenced by any stored manifest.
func GarbageCollect(blobStore *ociblob.Store, ociStore *OCIStore) (GCResult, error) {
	if blobStore == nil || ociStore == nil {
		return GCResult{}, fmt.Errorf("GC requires blob and OCI stores")
	}
	referenced, err := ociStore.ReferencedDigests()
	if err != nil {
		return GCResult{}, fmt.Errorf("GC load references: %w", err)
	}
	referencedSet := make(map[string]struct{}, len(referenced))
	for _, digest := range referenced {
		referencedSet[digest] = struct{}{}
	}
	blobs, err := blobStore.List()
	if err != nil {
		return GCResult{}, fmt.Errorf("GC list blobs: %w", err)
	}
	result := GCResult{}
	for _, digest := range blobs {
		if _, ok := referencedSet[digest]; ok {
			result.Kept++
			continue
		}
		if err := blobStore.Delete(digest); err != nil {
			return GCResult{}, fmt.Errorf("GC delete blob %s: %w", digest, err)
		}
		result.Removed++
	}
	return result, nil
}
