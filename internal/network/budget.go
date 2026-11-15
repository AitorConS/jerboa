package network

import "sync"

// sourceBudget bounds how many units of a shared resource one source (a
// guest's IP address) may hold at a time, so a single VM cannot take every
// slot of a pool the other VMs on its network share. The pool's own limit
// still applies on top of it.
type sourceBudget struct {
	mu    sync.Mutex
	limit int
	held  map[string]int
}

func newSourceBudget(limit int) *sourceBudget {
	return &sourceBudget{limit: limit, held: map[string]int{}}
}

// acquire takes one unit for source and reports whether it was within its
// share. Every successful acquire must be paired with one release.
func (b *sourceBudget) acquire(source string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.held[source] >= b.limit {
		return false
	}
	b.held[source]++
	return true
}

func (b *sourceBudget) release(source string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.held[source]--; b.held[source] <= 0 {
		delete(b.held, source)
	}
}
