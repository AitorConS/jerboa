package network

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceBudgetLeavesRoomForOtherSources(t *testing.T) {
	b := newSourceBudget(3)
	for range 3 {
		require.True(t, b.acquire("10.0.0.2"))
	}
	require.False(t, b.acquire("10.0.0.2"), "a source may not exceed its share")
	require.True(t, b.acquire("10.0.0.3"), "another source keeps its own share")

	b.release("10.0.0.2")
	require.True(t, b.acquire("10.0.0.2"), "a released unit can be taken again")

	for range 3 {
		b.release("10.0.0.2")
	}
	b.release("10.0.0.3")
	require.Empty(t, b.held, "sources holding nothing are forgotten")
}

func TestSourceBudgetConcurrent(t *testing.T) {
	b := newSourceBudget(8)
	var wg sync.WaitGroup
	var mu sync.Mutex
	peak, held := 0, 0
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !b.acquire("vm") {
				return
			}
			mu.Lock()
			held++
			peak = max(peak, held)
			mu.Unlock()
			mu.Lock()
			held--
			mu.Unlock()
			b.release("vm")
		}()
	}
	wg.Wait()
	require.LessOrEqual(t, peak, 8)
	require.Empty(t, b.held)
}
