package core

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestWaitForDispatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var inflight atomic.Int64
		start := time.Now()
		WaitForDispatch(t.Context(), &inflight)
		if time.Since(start) != 0 {
			t.Fatal("idle dispatch blocked")
		}

		inflight.Add(1)
		time.AfterFunc(time.Second, func() { inflight.Add(-1) })
		WaitForDispatch(t.Context(), &inflight)
		if inflight.Load() != 0 {
			t.Fatal("returned while a handler was running")
		}

		inflight.Add(1)
		canceled, stop := context.WithCancel(t.Context())
		stop()
		start = time.Now()
		WaitForDispatch(canceled, &inflight)
		if time.Since(start) != 0 || inflight.Load() != 1 {
			t.Fatal("canceled drain waited or changed the pending count")
		}

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		WaitForDispatch(ctx, &inflight)
		if ctx.Err() != context.DeadlineExceeded || inflight.Load() != 1 {
			t.Fatal("timed-out drain must leave the running handler counted")
		}
	})
}
