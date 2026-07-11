package core

import (
	"context"
	"errors"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// fakeAdapter is a controllable core.Adapter that lets tests drive every
// lifecycle path: Connect/Disconnect/Send/Attachments outcomes are configurable.
type fakeAdapter struct {
	connectErr error
	onConnect  func(ctx context.Context, deps AdapterDeps)

	disconnectErr   error
	disconnectCalls int

	sendErr error
	sent    []string

	attachments []Attachment
	attachErr   error
}

func (f *fakeAdapter) Connect(ctx context.Context, deps AdapterDeps) error {
	if f.connectErr != nil {
		return f.connectErr
	}
	if f.onConnect != nil {
		f.onConnect(ctx, deps)
	}
	return nil
}

func (f *fakeAdapter) Disconnect() error {
	f.disconnectCalls++
	return f.disconnectErr
}

func (f *fakeAdapter) Send(_ context.Context, _, text string, _ SendOptions) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, text)
	return nil
}

func (f *fakeAdapter) Attachments(*Message) ([]Attachment, error) {
	return f.attachments, f.attachErr
}

func TestBot_Connect_Success(t *testing.T) {
	fake := &fakeAdapter{}
	bot := New(SlackBotType, fake)

	asserts.NoError(t, bot.Connect(context.Background()), "first Connect")
	asserts.ErrorIs(t, bot.Connect(context.Background()), ErrAlreadyConnected, "second Connect")

	asserts.NoError(t, bot.Disconnect(), "Disconnect clears the connection")
	asserts.NoError(t, bot.Connect(context.Background()), "Connect again after Disconnect")

	asserts.NoError(t, bot.Disconnect(), "final teardown")
}

func TestBot_Connect_AdapterError(t *testing.T) {
	sentinel := errors.New("connect boom")
	fake := &fakeAdapter{connectErr: sentinel}
	bot := New(SlackBotType, fake)

	asserts.ErrorIs(t, bot.Connect(context.Background()), sentinel, "Connect surfaces the adapter error")

	// The failed Connect cleared the connection state, so a retry is possible.
	fake.connectErr = nil
	asserts.NoError(t, bot.Connect(context.Background()), "Connect after the error was cleared")

	asserts.NoError(t, bot.Disconnect(), "teardown")
}

func TestBot_Disconnect_NotConnected(t *testing.T) {
	bot := New(SlackBotType, &fakeAdapter{})

	asserts.NoError(t, bot.Disconnect(), "Disconnect with an adapter but no active connection")
}

func TestBot_Run_ConnectError(t *testing.T) {
	sentinel := errors.New("connect boom")
	fake := &fakeAdapter{connectErr: sentinel}
	bot := New(SlackBotType, fake)

	asserts.ErrorIs(t, bot.Run(context.Background()), sentinel, "Run returns the Connect error without blocking")
}

func TestBot_Run_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &fakeAdapter{onConnect: func(context.Context, AdapterDeps) {}}
	bot := New(SlackBotType, fake)

	asserts.NoError(t, bot.Run(ctx), "Run returns nil on a clean context cancellation")
	asserts.Equal(t, fake.disconnectCalls, 1, "adapter disconnected exactly once")
}

func TestBot_Run_LoopEnds(t *testing.T) {
	sentinel := errors.New("loop ended")
	fake := &fakeAdapter{onConnect: func(_ context.Context, deps AdapterDeps) {
		deps.Done(sentinel)
	}}
	bot := New(SlackBotType, fake)

	asserts.ErrorIs(t, bot.Run(context.Background()), sentinel, "Run returns the loop error")
}

func TestBot_Run_DisconnectErrorCleanLoop(t *testing.T) {
	sentinel := errors.New("disconnect boom")
	fake := &fakeAdapter{
		disconnectErr: sentinel,
		onConnect: func(_ context.Context, deps AdapterDeps) {
			deps.Done(nil)
		},
	}
	bot := New(SlackBotType, fake)

	asserts.ErrorIs(t, bot.Run(context.Background()), sentinel, "Run surfaces the disconnect error when the loop ended cleanly")
}

func TestBot_Run_DisconnectErrorDuringShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &fakeAdapter{
		disconnectErr: errors.New("disconnect boom"),
		onConnect:     func(context.Context, AdapterDeps) {},
	}
	bot := New(SlackBotType, fake)

	asserts.NoError(t, bot.Run(ctx), "a disconnect error during shutdown is only logged, not returned")
}

func TestBot_SendMessage_Success(t *testing.T) {
	fake := &fakeAdapter{}
	bot := New(SlackBotType, fake)

	asserts.NoError(t, bot.SendMessageContext(context.Background(), "chan", "hello ctx"), "SendMessageContext")
	asserts.NoError(t, bot.SendMessage("chan", "hello bg"), "SendMessage")

	asserts.Equal(t, len(fake.sent), 2, "both messages were sent")
	asserts.Equal(t, fake.sent[0], "hello ctx", "SendMessageContext text recorded")
	asserts.Equal(t, fake.sent[1], "hello bg", "SendMessage text recorded")
}

func TestBot_GetAttachments_Success(t *testing.T) {
	fake := &fakeAdapter{attachments: []Attachment{{URL: "a"}, {URL: "b", IsImage: true}}}
	bot := New(SlackBotType, fake)

	got, err := bot.GetAttachments(&Message{})

	asserts.NoError(t, err, "GetAttachments success")
	asserts.Equal(t, len(got), 2, "two attachments returned")
	asserts.Equal(t, got[0].URL, "a", "first attachment URL")
	asserts.True(t, got[1].IsImage, "second attachment is an image")
}

func TestBot_Start(t *testing.T) {
	// An immediate loop-end lets Start (which wraps Run with a signal context)
	// return deterministically without an OS signal.
	fake := &fakeAdapter{onConnect: func(_ context.Context, deps AdapterDeps) {
		deps.Done(nil)
	}}
	bot := New(SlackBotType, fake)

	asserts.NoError(t, bot.Start(), "Start returns when the event loop ends")
}
