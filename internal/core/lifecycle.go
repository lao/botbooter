package core

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// A connection owns the callbacks and cancellation signals for one run.
type connection struct {
	cancel         context.CancelFunc
	loopResult     chan error
	stopped        <-chan struct{}
	disconnectOnce sync.Once
	disconnectErr  error
	adapter        Adapter
	// Closed when the optional conversation sweeper exits.
	sweeperDone <-chan struct{}
}

// Even a superseded connection must wake its Run. Only the current owner
// may disconnect the shared adapter, and concurrent callers share its result.
func (c *connection) teardown(disconnectAdapter bool) error {
	c.cancel()
	if !disconnectAdapter {
		return nil
	}
	if c.adapter == nil {
		return ErrUnknownBotType
	}
	c.disconnectOnce.Do(func() { c.disconnectErr = c.adapter.Disconnect() })
	return c.disconnectErr
}

// Connect starts the adapter's event loop and returns without blocking. It
// returns ErrAlreadyConnected if a connection is already active, ErrUnknownBotType
// if the Bot has no adapter, every registration error recorded by AddHandler
// (joined, one per invalid pattern), or any error from the adapter's own Connect.
func (b *Bot) Connect(ctx context.Context) error {
	_, err := b.connect(ctx)
	return err
}

func (b *Bot) connect(ctx context.Context) (*connection, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		return nil, ErrAlreadyConnected
	}
	if b.adapter == nil {
		return nil, ErrUnknownBotType
	}
	if err := errors.Join(b.setupErrs...); err != nil {
		return nil, err
	}
	chain := b.composeDispatchChain()
	b.dispatchChain.Store(&chain)
	runCtx, cancel := context.WithCancel(ctx)
	c := &connection{
		cancel:     cancel,
		loopResult: make(chan error, 1),
		stopped:    runCtx.Done(),
		adapter:    b.adapter,
	}

	// Bind callbacks to this connection so a late callback cannot act on a successor.
	deps := AdapterDeps{
		Dispatch:            b.dispatch,
		DispatchReaction:    b.dispatchReaction,
		HasReactionHandlers: len(b.reactionHandlers) > 0,
		Done:                func(err error) { c.loopResult <- err },
		Disconnect:          func() error { return b.disconnectConn(c) },
		Logger:              b.log(),
	}

	// Keep Disconnect behind this lock until adapter setup succeeds or fails.
	if err := b.adapter.Connect(runCtx, deps); err != nil {
		cancel()
		return nil, err
	}
	b.conn = c

	// State survives reconnects. Sweepers may briefly overlap, with expiry checked under lock.
	if b.conversations != nil && len(b.flows) > 0 {
		c.sweeperDone = b.conversations.startSweeper(runCtx, defaultSweepInterval)
	}
	return c, nil
}

// Keep the current connection installed until its adapter finishes disconnecting.
// This prevents a new Connect from starting on a half-drained adapter.
func (b *Bot) disconnectConn(c *connection) error {
	b.mu.Lock()
	current := b.conn == c
	b.mu.Unlock()

	err := c.teardown(current)

	if current {
		b.mu.Lock()
		if b.conn == c {
			b.conn = nil
		}
		b.mu.Unlock()
	}
	return err
}

// Disconnect tears down the active connection: it cancels the run context and
// runs the adapter's Disconnect exactly once. It is safe to call when not
// connected, returning ErrUnknownBotType only if the Bot has no adapter.
func (b *Bot) Disconnect() error {
	b.mu.Lock()
	c := b.conn
	b.mu.Unlock()

	if c == nil {
		if b.adapter == nil {
			return ErrUnknownBotType
		}
		return nil
	}
	return b.disconnectConn(c)
}

// Run connects the Bot and blocks until ctx is canceled, the event loop ends,
// or Disconnect is called from elsewhere, then disconnects. A clean shutdown
// (ctx cancellation or a local Disconnect) returns nil rather than ctx.Err(),
// so callers can safely do log.Fatal(bot.Run(ctx)).
func (b *Bot) Run(ctx context.Context) error {
	c, err := b.connect(ctx)
	if err != nil {
		return err
	}

	var loopErr error
	select {
	case <-c.stopped:
	case loopErr = <-c.loopResult:
	}

	disconnectErr := b.disconnectConn(c)

	// An event loop may echo cancellation during a clean shutdown.
	if errors.Is(loopErr, context.Canceled) ||
		(ctx.Err() != nil && errors.Is(loopErr, ctx.Err())) {
		loopErr = nil
	}

	if disconnectErr != nil {
		if ctx.Err() != nil {
			b.log().Error("botbooter: error disconnecting during shutdown", "error", disconnectErr)
		} else if loopErr == nil {
			loopErr = disconnectErr
		}
	}
	return loopErr
}

// Start runs the Bot until the process receives an interrupt or SIGTERM.
func (b *Bot) Start() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return b.Run(ctx)
}
