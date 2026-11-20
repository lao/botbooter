package core

import (
	"context"
	"sync/atomic"
	"time"
)

// WaitForDispatch waits for inflight to reach zero or ctx to expire. Adapters
// stop accepting work before draining and own the counter across the wait.
func WaitForDispatch(ctx context.Context, inflight *atomic.Int64) {
	for inflight.Load() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}
