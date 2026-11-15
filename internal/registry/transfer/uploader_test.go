package transfer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AitorConS/jerboa/internal/registry/pack"
	"github.com/stretchr/testify/require"
)

func packOf(size int) pack.Pack { return pack.Pack{Data: make([]byte, size)} }

func TestPackUploaderBoundsBytesInFlight(t *testing.T) {
	var inFlight, peak, large atomic.Int64
	var done atomic.Int32
	put := func(_ context.Context, p pack.Pack) error {
		now := inFlight.Add(int64(len(p.Data)))
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		if len(p.Data) > pushInFlightBytes && now != int64(len(p.Data)) {
			large.Add(1) // a large pack shared the link
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-int64(len(p.Data)))
		done.Add(1)
		return nil
	}
	u := newPackUploader(context.Background(), put)
	packs := []pack.Pack{}
	for range 8 {
		packs = append(packs, packOf(pack.TargetSize))
	}
	packs = append(packs, packOf(pack.MaxSize), packOf(pack.TargetSize))
	start := time.Now()
	require.NoError(t, u.Add(packs))
	require.NoError(t, u.Wait())
	require.EqualValues(t, len(packs), done.Load())
	require.Zero(t, large.Load(), "a pack over the bound must upload alone")
	require.EqualValues(t, pack.MaxSize, peak.Load())
	// Ten uploads of 50 ms each take 500 ms in series and about 250 ms here.
	require.Less(t, time.Since(start), 400*time.Millisecond, "packs of the usual size upload three at a time")
}

func TestPackUploaderStopsAtTheFirstFailure(t *testing.T) {
	boom := errors.New("push: pack: registry said no")
	var started atomic.Int32
	var once sync.Once
	put := func(ctx context.Context, _ pack.Pack) error {
		started.Add(1)
		var err error
		once.Do(func() { err = boom })
		if err != nil {
			return err
		}
		<-ctx.Done() // the others see the push being canceled
		return ctx.Err()
	}
	u := newPackUploader(context.Background(), put)
	packs := []pack.Pack{}
	for range 20 {
		packs = append(packs, packOf(pack.TargetSize))
	}
	err := u.Add(packs)
	if err == nil {
		err = u.Wait()
	}
	require.ErrorIs(t, err, boom)
	require.Less(t, started.Load(), int32(20), "no upload starts after one failed")
}

func TestPackUploaderAbandonKeepsTheCallersError(t *testing.T) {
	u := newPackUploader(context.Background(), func(ctx context.Context, _ pack.Pack) error {
		<-ctx.Done()
		return ctx.Err()
	})
	require.NoError(t, u.Add([]pack.Pack{packOf(1)}))
	mine := errors.New("push: blob changed")
	require.ErrorIs(t, u.Abandon(mine), mine)
}
