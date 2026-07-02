package slack

import (
	"sync"
	"testing"
	"time"

	"github.com/lao/botbooter/internal/asserts"
)

// The dispatcher must run queued handlers to completion when Disconnect drains,
// rather than abandoning acked work at shutdown.
func TestDisconnect_DrainsQueuedDispatch(t *testing.T) {
	a := newTestAdapter()
	queue := a.startDispatcher(func() {})

	ran := make(chan struct{})
	queue <- func() { close(ran) }
	close(queue) // the event pump closes the queue on shutdown

	asserts.NoError(t, a.Disconnect(), "drain completes without timeout")
	select {
	case <-ran:
	default:
		t.Fatal("queued dispatch did not run before Disconnect returned")
	}
}

// Handlers run in the order they were enqueued — a single dispatcher goroutine,
// not one goroutine per event.
func TestDispatch_InOrder(t *testing.T) {
	a := newTestAdapter()
	queue := a.startDispatcher(func() {})

	var mu sync.Mutex
	var order []int
	for i := 0; i < 5; i++ {
		i := i
		queue <- func() {
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
		}
	}
	close(queue)
	_ = a.Disconnect()

	asserts.Equal(t, len(order), 5, "all handlers ran")
	for i := range order {
		asserts.Equal(t, order[i], i, "handlers ran in enqueue order")
	}
}

// A wedged handler must not block shutdown forever: Disconnect returns an error
// once the drain deadline elapses.
func TestDisconnect_DrainTimeout(t *testing.T) {
	old := dispatchDrainTimeout
	dispatchDrainTimeout = 30 * time.Millisecond
	defer func() { dispatchDrainTimeout = old }()

	a := newTestAdapter()
	queue := a.startDispatcher(func() {})

	block := make(chan struct{})
	queue <- func() { <-block } // never returns until released; ignores cancel

	err := a.Disconnect()
	asserts.Error(t, err, "drain times out on a wedged handler")
	close(block)
}
