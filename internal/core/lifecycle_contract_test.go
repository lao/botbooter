package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lao/botbooter/internal/asserts"
)

// watcherAdapter models the discord/whatsapp/teams shape: Connect spawns a
// goroutine that tears the connection down when its run context is canceled.
// This is the exact structure that leaked a stale connection's teardown onto a
// successor before the connection-scoped Disconnect fix.
type watcherAdapter struct {
	mu          sync.Mutex
	disconnects int
}

func (a *watcherAdapter) Connect(ctx context.Context, deps AdapterDeps) error {
	go func() {
		<-ctx.Done()
		_ = deps.Disconnect()
	}()
	return nil
}

func (a *watcherAdapter) Disconnect() error {
	a.mu.Lock()
	a.disconnects++
	a.mu.Unlock()
	return nil
}

func (a *watcherAdapter) Send(ctx context.Context, channelID, text string, opts SendOptions) error {
	return nil
}

func (a *watcherAdapter) Attachments(m *Message) ([]Attachment, error) { return nil, nil }

func (a *watcherAdapter) disconnectCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.disconnects
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

func (b *Bot) currentConn() *connection {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn
}

// A Disconnect from another goroutine while Run blocks must wake Run and return
// nil — the local-teardown case Run previously never observed (C2).
func TestLifecycle_DisconnectDuringRunReturnsNil(t *testing.T) {
	b := New(SlackBotType, &watcherAdapter{})
	errCh := make(chan error, 1)
	go func() { errCh <- b.Run(context.Background()) }()

	waitFor(t, func() bool { return b.currentConn() != nil }, "Run to connect")

	asserts.NoError(t, b.Disconnect(), "external Disconnect during Run")

	select {
	case runErr := <-errCh:
		asserts.NoError(t, runErr, "Run returns nil after a local Disconnect")
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Disconnect — it hung")
	}
}

// A stale watcher from a disconnected connection must not tear down the
// successor connection installed by a reconnect (C3), and must not double-call
// the shared adapter's Disconnect.
func TestLifecycle_StaleWatcherDoesNotCancelSuccessor(t *testing.T) {
	a := &watcherAdapter{}
	b := New(SlackBotType, a)

	asserts.NoError(t, b.Connect(context.Background()), "connect 1")
	c1 := b.currentConn()

	// Disconnecting connection 1 cancels its run context; its watcher will fire
	// deps.Disconnect asynchronously.
	asserts.NoError(t, b.Disconnect(), "disconnect 1")

	asserts.NoError(t, b.Connect(context.Background()), "connect 2 (reconnect)")
	c2 := b.currentConn()
	asserts.True(t, c1 != c2, "reconnect installs a distinct connection")

	// Give connection 1's stale watcher ample time to fire against the seam.
	time.Sleep(50 * time.Millisecond)

	asserts.True(t, b.currentConn() == c2, "successor still installed after stale watcher fired")
	select {
	case <-c2.runDone:
		t.Fatal("stale watcher tore down the successor connection")
	default:
	}

	asserts.NoError(t, b.Disconnect(), "disconnect 2")
	// Exactly two real teardowns (conn1 + conn2); the stale watcher's superseded
	// teardown must not have touched the shared adapter.
	asserts.Equal(t, a.disconnectCount(), 2, "adapter Disconnect called once per real connection")
}

// blockingAdapter blocks inside Connect until released, modeling an adapter
// whose Connect performs a slow (network) dial — e.g. discordgo's Session.Open.
type blockingAdapter struct {
	enteredOnce sync.Once
	entered     chan struct{} // closed when Connect is first entered
	release     chan struct{}
	mu          sync.Mutex
	disconnects int
}

func (a *blockingAdapter) Connect(ctx context.Context, deps AdapterDeps) error {
	a.enteredOnce.Do(func() { close(a.entered) })
	<-a.release
	return nil
}

func (a *blockingAdapter) Disconnect() error {
	a.mu.Lock()
	a.disconnects++
	a.mu.Unlock()
	return nil
}

func (a *blockingAdapter) Send(ctx context.Context, channelID, text string, opts SendOptions) error {
	return nil
}

func (a *blockingAdapter) Attachments(m *Message) ([]Attachment, error) { return nil, nil }

func (a *blockingAdapter) disconnectCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.disconnects
}

// A Disconnect that races a still-in-flight Connect must disconnect the adapter
// exactly once, never twice — the double-invocation the connection-scoped
// teardown must prevent.
func TestLifecycle_DisconnectRacingConnectDisconnectsOnce(t *testing.T) {
	a := &blockingAdapter{entered: make(chan struct{}), release: make(chan struct{})}
	b := New(SlackBotType, a)

	connErr := make(chan error, 1)
	go func() { connErr <- b.Connect(context.Background()) }()
	// Wait until Connect has entered adapter.Connect (holding b.mu).
	<-a.entered

	discErr := make(chan error, 1)
	go func() { discErr <- b.Disconnect() }()
	// Blocking on b.mu is not observable from outside, so give Disconnect a
	// moment to reach the lock (best effort — either interleaving must satisfy
	// the assertions), then release Connect so it installs the connection and
	// unlocks, letting Disconnect proceed.
	time.Sleep(20 * time.Millisecond)
	close(a.release)

	asserts.NoError(t, <-connErr, "Connect")
	asserts.NoError(t, <-discErr, "Disconnect")
	asserts.Equal(t, a.disconnectCount(), 1, "adapter Disconnect called exactly once under the race")
	asserts.True(t, b.currentConn() == nil, "no live connection after Connect/Disconnect race")
}

// Reconnecting after a clean Run/Disconnect cycle must succeed rather than
// report ErrAlreadyConnected from leftover state.
func TestLifecycle_ReconnectAfterDisconnect(t *testing.T) {
	b := New(SlackBotType, &watcherAdapter{})
	asserts.NoError(t, b.Connect(context.Background()), "connect 1")
	asserts.NoError(t, b.Disconnect(), "disconnect 1")
	asserts.NoError(t, b.Connect(context.Background()), "connect 2")
	asserts.NoError(t, b.Disconnect(), "disconnect 2")
}

// slowDisconnectAdapter blocks inside adapter.Disconnect until release is
// closed, modeling the webhook adapters' multi-second shutdown drains.
type slowDisconnectAdapter struct {
	enteredOnce sync.Once
	entered     chan struct{} // closed when Disconnect is first entered
	release     chan struct{} // Disconnect blocks until this is closed
}

func (a *slowDisconnectAdapter) Connect(ctx context.Context, deps AdapterDeps) error { return nil }

func (a *slowDisconnectAdapter) Disconnect() error {
	a.enteredOnce.Do(func() { close(a.entered) })
	<-a.release
	return nil
}

func (a *slowDisconnectAdapter) Send(ctx context.Context, channelID, text string, opts SendOptions) error {
	return nil
}

func (a *slowDisconnectAdapter) Attachments(m *Message) ([]Attachment, error) { return nil, nil }

// A slow adapter Disconnect (webhook drains run for seconds) must not open a
// window where a concurrent Connect starts a second live session on the shared
// adapter: until the teardown completes, Connect reports ErrAlreadyConnected,
// and a reconnect succeeds only once Disconnect has returned.
func TestLifecycle_ConnectDuringSlowDisconnectIsRejected(t *testing.T) {
	a := &slowDisconnectAdapter{entered: make(chan struct{}), release: make(chan struct{})}
	b := New(SlackBotType, a)
	asserts.NoError(t, b.Connect(context.Background()), "connect 1")

	discErr := make(chan error, 1)
	go func() { discErr <- b.Disconnect() }()
	<-a.entered // adapter.Disconnect is now in-flight

	asserts.ErrorIs(t, b.Connect(context.Background()), ErrAlreadyConnected,
		"Connect during an in-flight Disconnect")

	close(a.release)
	asserts.NoError(t, <-discErr, "disconnect 1")
	asserts.NoError(t, b.Connect(context.Background()), "reconnect after teardown completed")
	asserts.NoError(t, b.Disconnect(), "disconnect 2")
}
