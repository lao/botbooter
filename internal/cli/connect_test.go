package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

func recvMessage(t *testing.T, ch <-chan *core.Message) *core.Message {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatched message")
		return nil
	}
}

func TestConnect_DispatchesLines(t *testing.T) {
	a := newAdapter(strings.NewReader("hello\nworld\n"), io.Discard)
	msgs := make(chan *core.Message, 2)
	done := make(chan error, 1)
	deps := core.AdapterDeps{
		Dispatch: func(_ context.Context, m *core.Message) { msgs <- m },
		Done:     func(err error) { done <- err },
	}

	asserts.NoError(t, a.Connect(context.Background(), deps), "connect")

	asserts.Equal(t, recvMessage(t, msgs).Content, "hello", "first line")
	asserts.Equal(t, recvMessage(t, msgs).Content, "world", "second line")

	select {
	case err := <-done:
		asserts.NoError(t, err, "done error after EOF")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for done")
	}
}

// A context cancelled before Connect must stop the read loop without dispatching.
func TestConnect_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a := newAdapter(strings.NewReader("late\n"), io.Discard)
	msgs := make(chan *core.Message, 1)
	deps := core.AdapterDeps{
		Dispatch: func(_ context.Context, m *core.Message) { msgs <- m },
		Done:     func(error) {},
	}

	asserts.NoError(t, a.Connect(ctx, deps), "connect")

	select {
	case m := <-msgs:
		t.Fatalf("dispatched %q despite cancelled context", m.Content)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSend_WritesLine(t *testing.T) {
	var buf bytes.Buffer
	a := newAdapter(nil, &buf)

	asserts.NoError(t, a.Send(context.Background(), "", "hi"), "send")
	asserts.Equal(t, buf.String(), "hi\n", "written line")
}

var errFailedWrite = errors.New("write failed")

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errFailedWrite }

func TestSend_WriterError(t *testing.T) {
	a := newAdapter(nil, errWriter{})

	err := a.Send(context.Background(), "", "hi")
	asserts.ErrorIs(t, err, errFailedWrite, "send surfaces writer error")
}

func TestDisconnect(t *testing.T) {
	a := newAdapter(nil, io.Discard)

	asserts.NoError(t, a.Disconnect(), "disconnect")
}

func TestAttachments(t *testing.T) {
	a := newAdapter(nil, io.Discard)

	att := core.Attachment{URL: "/tmp/pic.png", IsImage: true}
	m := &core.Message{Raw: &core.CLIMessage{Attachments: []core.Attachment{att}}}

	got, err := a.Attachments(m)
	asserts.NoError(t, err, "attachments")
	asserts.Equal(t, len(got), 1, "one attachment")
	asserts.Equal(t, got[0].URL, att.URL, "attachment url")

	got, err = a.Attachments(&core.Message{})
	asserts.NoError(t, err, "attachments with no CLI raw")
	asserts.True(t, got == nil, "nil attachments when Raw is unset")
}
