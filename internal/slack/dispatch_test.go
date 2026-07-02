package slack

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// The event pump acks and enqueues an EventsAPI message event for dispatch,
// exercising Connect's loop hermetically via the exported Events channel
// without any network connection.
func TestPumpEvents_EnqueuesDispatch(t *testing.T) {
	a := newTestAdapter()
	events := make(chan socketmode.Event, 1)
	queue := make(chan func(), 1)

	var got *core.Message
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.pumpEvents(ctx, context.Background(), events, queue, captureDeps(&got))

	events <- socketmode.Event{
		Type: socketmode.EventTypeEventsAPI,
		Data: slackEvent(&slackevents.MessageEvent{Text: "hi", User: "U1", Channel: "C1"}),
	}

	select {
	case fn := <-queue:
		fn() // the dispatcher would run this; invoke it directly to observe dispatch
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not enqueue a dispatch")
	}
	asserts.NotNil(t, got, "event dispatched through the pump")
}

// A canceled context stops the pump promptly even with no events pending.
func TestPumpEvents_StopsOnCancel(t *testing.T) {
	a := newTestAdapter()
	events := make(chan socketmode.Event)
	queue := make(chan func(), 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.pumpEvents(ctx, context.Background(), events, queue, captureDeps(nil))
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not stop on context cancellation")
	}
}

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
