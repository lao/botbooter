package signal

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// validConfig returns a Config with every required field populated. Address is
// overwritten by tests that talk to a fake daemon.
func validConfig() Config {
	return Config{Address: "127.0.0.1:1", Account: "+15550001"}
}

// startDaemon starts a fake signal-cli JSON-RPC TCP daemon and returns its
// address plus a channel yielding accepted connections.
func startDaemon(t *testing.T) (string, <-chan net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	asserts.NoError(t, err, "fake daemon listen")
	t.Cleanup(func() { _ = ln.Close() })
	conns := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns <- c
		}
	}()
	return ln.Addr().String(), conns
}

// acceptConn waits for the daemon to accept the adapter's dial.
func acceptConn(t *testing.T, conns <-chan net.Conn) net.Conn {
	t.Helper()
	select {
	case c := <-conns:
		t.Cleanup(func() { _ = c.Close() })
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for adapter to dial")
		return nil
	}
}

// captureDeps returns AdapterDeps recording dispatches into *got and signaling
// done per dispatch, plus routing Done errors into loopErr when non-nil.
func captureDeps(got *[]*core.Message, done chan<- struct{}, loopErr chan<- error) core.AdapterDeps {
	return core.AdapterDeps{
		Dispatch: func(_ context.Context, m *core.Message) {
			*got = append(*got, m)
			if done != nil {
				done <- struct{}{}
			}
		},
		Done: func(err error) {
			if loopErr != nil {
				loopErr <- err
			}
		},
		Disconnect: func() error { return nil },
	}
}

// connectAdapter dials a via bot-independent Connect and returns the daemon's
// side of the connection.
func connectAdapter(t *testing.T, a *adapter, deps core.AdapterDeps, addr string, conns <-chan net.Conn) net.Conn {
	t.Helper()
	a.cfg.Address = addr
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	asserts.NoError(t, a.Connect(ctx, deps), "Connect")
	t.Cleanup(func() { _ = a.Disconnect() })
	return acceptConn(t, conns)
}

// writeLine writes one newline-terminated JSON-RPC frame to the adapter.
func writeLine(t *testing.T, c net.Conn, line string) {
	t.Helper()
	_, err := c.Write([]byte(line + "\n"))
	asserts.NoError(t, err, "daemon write")
}

// readRequest reads one JSON-RPC request line sent by the adapter.
func readRequest(t *testing.T, r *bufio.Reader) map[string]any {
	t.Helper()
	line, err := r.ReadString('\n')
	asserts.NoError(t, err, "daemon read request")
	var req map[string]any
	asserts.NoError(t, json.Unmarshal([]byte(line), &req), "unmarshal request")
	return req
}

func awaitDispatch(t *testing.T, done <-chan struct{}, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for dispatch %d of %d", i+1, n)
		}
	}
}

const directReceive = `{"jsonrpc":"2.0","method":"receive","params":{"account":"+15550001","envelope":{"source":"+15550002","sourceNumber":"+15550002","sourceUuid":"uuid-2","sourceName":"Ada","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"hello bot"}}}}`

const groupReceive = `{"jsonrpc":"2.0","method":"receive","params":{"account":"+15550001","envelope":{"source":"+15550002","sourceNumber":"+15550002","sourceName":"Ada","timestamp":1700000000456,"dataMessage":{"timestamp":1700000000456,"message":"hi group","groupInfo":{"groupId":"grp==","type":"DELIVER"}}}}}`

const receiptReceive = `{"jsonrpc":"2.0","method":"receive","params":{"account":"+15550001","envelope":{"source":"+15550002","timestamp":1700000000789,"receiptMessage":{"when":1700000000789,"isDelivery":true}}}}`

const ownReceive = `{"jsonrpc":"2.0","method":"receive","params":{"account":"+15550001","envelope":{"source":"+15550001","sourceNumber":"+15550001","timestamp":1700000001000,"dataMessage":{"timestamp":1700000001000,"message":"from myself"}}}}`

const attachmentReceive = `{"jsonrpc":"2.0","method":"receive","params":{"account":"+15550001","envelope":{"source":"+15550002","sourceNumber":"+15550002","sourceName":"Ada","timestamp":1700000002000,"dataMessage":{"timestamp":1700000002000,"message":"a cat","attachments":[{"contentType":"image/jpeg","filename":"cat.jpg","id":"att-1","size":1234}]}}}}`

func TestNew(t *testing.T) {
	bot, err := New(validConfig())

	asserts.NoError(t, err, "New with full config should succeed")
	asserts.NotNil(t, bot, "bot should be initialized")
	asserts.Equal(t, bot.BotType, core.SignalBotType, "bot type should be Signal")
	asserts.Equal(t, bot.BotType.String(), "signal", "bot type string should be signal")
}

func TestNew_MissingAddress(t *testing.T) {
	_, err := New(Config{Account: "+15550001"})
	asserts.ErrorIs(t, err, ErrMissingConfig, "New without Address should fail")
}

func TestNew_Defaults(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter should succeed")
	asserts.True(t, a.cfg.DialTimeout > 0, "DialTimeout should default")
}

func TestConnect_DialError(t *testing.T) {
	cfg := validConfig()
	cfg.DialTimeout = 200 * time.Millisecond
	a, err := newAdapter(cfg)
	asserts.NoError(t, err, "newAdapter")

	err = a.Connect(context.Background(), core.AdapterDeps{})
	asserts.Error(t, err, "Connect to a dead address should fail")
}

func TestReceive_DispatchesDirectMessage(t *testing.T) {
	addr, conns := startDaemon(t)
	a, _ := newAdapter(validConfig())
	var got []*core.Message
	done := make(chan struct{}, 4)
	daemon := connectAdapter(t, a, captureDeps(&got, done, nil), addr, conns)

	writeLine(t, daemon, directReceive)
	awaitDispatch(t, done, 1)

	m := got[0]
	asserts.Equal(t, m.UserID, "+15550002", "UserID should be the sender number")
	asserts.Equal(t, m.AuthorName, "Ada", "AuthorName should be the source name")
	asserts.Equal(t, m.ChannelID, "+15550002", "direct ChannelID should be the sender")
	asserts.Equal(t, m.Content, "hello bot", "Content should be the message text")
	asserts.Equal(t, m.ID, "1700000000123", "ID should be the envelope timestamp")
	asserts.Equal(t, m.Timestamp.UnixMilli(), int64(1700000000123), "Timestamp should parse from millis")

	sm, ok := RawMessage(m)
	asserts.True(t, ok, "Raw should carry a signal Message")
	asserts.Equal(t, sm.Source, "+15550002", "raw Source")
	asserts.Equal(t, sm.Text, "hello bot", "raw Text")
}

func TestReceive_DispatchesGroupMessage(t *testing.T) {
	addr, conns := startDaemon(t)
	a, _ := newAdapter(validConfig())
	var got []*core.Message
	done := make(chan struct{}, 4)
	daemon := connectAdapter(t, a, captureDeps(&got, done, nil), addr, conns)

	writeLine(t, daemon, groupReceive)
	awaitDispatch(t, done, 1)

	asserts.Equal(t, got[0].ChannelID, "group:grp==", "group ChannelID should be prefixed")
	asserts.Equal(t, got[0].UserID, "+15550002", "UserID should stay the individual sender")
	sm, _ := RawMessage(got[0])
	asserts.Equal(t, sm.GroupID, "grp==", "raw GroupID")
}

func TestReceive_SkipsNonDataAndOwnMessages(t *testing.T) {
	addr, conns := startDaemon(t)
	a, _ := newAdapter(validConfig())
	var got []*core.Message
	done := make(chan struct{}, 4)
	daemon := connectAdapter(t, a, captureDeps(&got, done, nil), addr, conns)

	// A receipt (no dataMessage), the bot's own message, then a real one. Only
	// the real one dispatches — and its arrival proves the loop survived the
	// first two.
	writeLine(t, daemon, receiptReceive)
	writeLine(t, daemon, ownReceive)
	writeLine(t, daemon, directReceive)
	awaitDispatch(t, done, 1)

	asserts.Equal(t, len(got), 1, "only the real inbound message should dispatch")
	asserts.Equal(t, got[0].Content, "hello bot", "the surviving message is the real one")
}

func TestReceive_SkipsMalformedLine(t *testing.T) {
	addr, conns := startDaemon(t)
	a, _ := newAdapter(validConfig())
	var got []*core.Message
	done := make(chan struct{}, 4)
	daemon := connectAdapter(t, a, captureDeps(&got, done, nil), addr, conns)

	writeLine(t, daemon, `{not json`)
	writeLine(t, daemon, `{"jsonrpc":"2.0","method":"receive","params":"not an object"}`)
	writeLine(t, daemon, directReceive)
	awaitDispatch(t, done, 1)

	asserts.Equal(t, len(got), 1, "malformed lines should be skipped, not fatal")
}

func TestSend_Direct(t *testing.T) {
	addr, conns := startDaemon(t)
	a, _ := newAdapter(validConfig())
	daemon := connectAdapter(t, a, captureDeps(nil, nil, nil), addr, conns)
	r := bufio.NewReader(daemon)

	sendErr := make(chan error, 1)
	go func() { sendErr <- a.Send(context.Background(), "+15550002", "hi there") }()

	req := readRequest(t, r)
	asserts.Equal(t, req["method"], "send", "method should be send")
	asserts.Equal(t, req["jsonrpc"], "2.0", "jsonrpc version")
	params := req["params"].(map[string]any)
	asserts.Equal(t, params["message"], "hi there", "message param")
	asserts.Equal(t, params["account"], "+15550001", "account param")
	recipients := params["recipient"].([]any)
	asserts.Equal(t, len(recipients), 1, "one recipient")
	asserts.Equal(t, recipients[0], "+15550002", "recipient number")

	id := int(req["id"].(float64))
	writeLine(t, daemon, `{"jsonrpc":"2.0","id":`+strconv.Itoa(id)+`,"result":{"timestamp":1700000003000}}`)
	asserts.NoError(t, <-sendErr, "Send should succeed on a result response")
}

func TestSend_Group(t *testing.T) {
	addr, conns := startDaemon(t)
	a, _ := newAdapter(validConfig())
	daemon := connectAdapter(t, a, captureDeps(nil, nil, nil), addr, conns)
	r := bufio.NewReader(daemon)

	sendErr := make(chan error, 1)
	go func() { sendErr <- a.Send(context.Background(), "group:grp==", "hi group") }()

	req := readRequest(t, r)
	params := req["params"].(map[string]any)
	asserts.Equal(t, params["groupId"], "grp==", "groupId param from prefixed channel")
	_, hasRecipient := params["recipient"]
	asserts.False(t, hasRecipient, "group send should carry no recipient")

	id := int(req["id"].(float64))
	writeLine(t, daemon, `{"jsonrpc":"2.0","id":`+strconv.Itoa(id)+`,"result":{}}`)
	asserts.NoError(t, <-sendErr, "group Send should succeed")
}

func TestSend_ErrorResponse(t *testing.T) {
	addr, conns := startDaemon(t)
	a, _ := newAdapter(validConfig())
	daemon := connectAdapter(t, a, captureDeps(nil, nil, nil), addr, conns)
	r := bufio.NewReader(daemon)

	sendErr := make(chan error, 1)
	go func() { sendErr <- a.Send(context.Background(), "+15550002", "hi") }()

	req := readRequest(t, r)
	id := int(req["id"].(float64))
	writeLine(t, daemon, `{"jsonrpc":"2.0","id":`+strconv.Itoa(id)+`,"error":{"code":-32602,"message":"Unregistered user"}}`)

	err := <-sendErr
	asserts.Error(t, err, "Send should surface a JSON-RPC error")
	asserts.True(t, strings.Contains(err.Error(), "Unregistered user"), "error should carry the daemon message")
}

func TestSend_ContextCanceled(t *testing.T) {
	addr, conns := startDaemon(t)
	a, _ := newAdapter(validConfig())
	daemon := connectAdapter(t, a, captureDeps(nil, nil, nil), addr, conns)
	r := bufio.NewReader(daemon)

	ctx, cancel := context.WithCancel(context.Background())
	sendErr := make(chan error, 1)
	go func() { sendErr <- a.Send(ctx, "+15550002", "hi") }()

	readRequest(t, r) // request went out; never answer it
	cancel()
	asserts.ErrorIs(t, <-sendErr, context.Canceled, "Send should honor ctx cancellation")
}

func TestSend_NotConnected(t *testing.T) {
	a, _ := newAdapter(validConfig())
	asserts.ErrorIs(t, a.Send(context.Background(), "+15550002", "hi"), ErrNotConnected,
		"Send before Connect should fail with ErrNotConnected")
}

func TestSend_ConnClosedWhilePending(t *testing.T) {
	addr, conns := startDaemon(t)
	a, _ := newAdapter(validConfig())
	daemon := connectAdapter(t, a, captureDeps(nil, nil, make(chan error, 1)), addr, conns)
	r := bufio.NewReader(daemon)

	sendErr := make(chan error, 1)
	go func() { sendErr <- a.Send(context.Background(), "+15550002", "hi") }()

	readRequest(t, r)
	_ = daemon.Close() // daemon dies with the request pending

	select {
	case err := <-sendErr:
		asserts.Error(t, err, "a pending Send must fail when the connection drops")
	case <-time.After(2 * time.Second):
		t.Fatal("Send hung after connection loss")
	}
}

func TestReadLoop_RemoteCloseReportsDone(t *testing.T) {
	addr, conns := startDaemon(t)
	a, _ := newAdapter(validConfig())
	loopErr := make(chan error, 1)
	daemon := connectAdapter(t, a, captureDeps(nil, nil, loopErr), addr, conns)

	_ = daemon.Close()

	select {
	case err := <-loopErr:
		asserts.Error(t, err, "an unexpected remote close should surface via Done")
	case <-time.After(2 * time.Second):
		t.Fatal("Done was never called after remote close")
	}
}

func TestDisconnect_CleanNoDone(t *testing.T) {
	addr, conns := startDaemon(t)
	a, _ := newAdapter(validConfig())
	loopErr := make(chan error, 1)
	connectAdapter(t, a, captureDeps(nil, nil, loopErr), addr, conns)

	asserts.NoError(t, a.Disconnect(), "clean Disconnect should succeed")

	select {
	case err := <-loopErr:
		t.Fatalf("Done should not fire on intentional Disconnect, got %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	asserts.NoError(t, a.Disconnect(), "Disconnect when already disconnected is a no-op")
}

func TestDisconnect_DrainsInflightDispatch(t *testing.T) {
	addr, conns := startDaemon(t)
	a, _ := newAdapter(validConfig())

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	finished := make(chan struct{}, 1)
	deps := core.AdapterDeps{
		Dispatch: func(_ context.Context, _ *core.Message) {
			entered <- struct{}{}
			<-release
			finished <- struct{}{}
		},
		Done:       func(error) {},
		Disconnect: func() error { return nil },
	}
	daemon := connectAdapter(t, a, deps, addr, conns)

	writeLine(t, daemon, directReceive)
	<-entered

	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()
	asserts.NoError(t, a.Disconnect(), "Disconnect should wait for the in-flight dispatch")

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("Disconnect returned without draining the in-flight dispatch")
	}
}

// TestDisconnect_WaitsForReadLoopExit pins the drain's happens-before edge:
// Disconnect must not return until the read loop has stopped consuming
// buffered frames, since only then is the inflight counter complete.
func TestDisconnect_WaitsForReadLoopExit(t *testing.T) {
	addr, conns := startDaemon(t)
	a, _ := newAdapter(validConfig())
	connectAdapter(t, a, captureDeps(nil, nil, nil), addr, conns)

	c := a.currentConn()
	asserts.NoError(t, a.Disconnect(), "Disconnect should succeed")

	select {
	case <-c.loopDone:
	default:
		t.Fatal("Disconnect returned before the read loop exited")
	}
}

func TestAttachments(t *testing.T) {
	addr, conns := startDaemon(t)
	a, _ := newAdapter(validConfig())
	var got []*core.Message
	done := make(chan struct{}, 4)
	daemon := connectAdapter(t, a, captureDeps(&got, done, nil), addr, conns)

	writeLine(t, daemon, attachmentReceive)
	awaitDispatch(t, done, 1)

	atts, err := a.Attachments(got[0])
	asserts.NoError(t, err, "Attachments should succeed")
	asserts.Equal(t, len(atts), 1, "one attachment")
	asserts.True(t, atts[0].IsImage, "image/jpeg should be flagged as image")
	sa, ok := atts[0].ExtraData.(*Attachment)
	asserts.True(t, ok, "ExtraData should carry the *Attachment")
	asserts.Equal(t, sa.ID, "att-1", "attachment id")
	asserts.Equal(t, sa.ContentType, "image/jpeg", "attachment content type")
	asserts.Equal(t, sa.Filename, "cat.jpg", "attachment filename")
	asserts.Equal(t, sa.Size, int64(1234), "attachment size")
}

func TestAttachments_NonSignalMessage(t *testing.T) {
	a, _ := newAdapter(validConfig())
	atts, err := a.Attachments(&core.Message{Raw: "not signal"})
	asserts.NoError(t, err, "foreign message should not error")
	asserts.True(t, atts == nil, "foreign message should yield nil attachments")
}

func TestRawMessage_NotSignal(t *testing.T) {
	_, ok := RawMessage(&core.Message{Raw: 42})
	asserts.False(t, ok, "RawMessage should report non-signal Raw")
}

// TestBot_EndToEnd drives the full core.Bot lifecycle against the fake daemon:
// connect, receive a message, reply from the handler, then shut down cleanly.
func TestBot_EndToEnd(t *testing.T) {
	addr, conns := startDaemon(t)
	cfg := validConfig()
	cfg.Address = addr
	bot, err := New(cfg)
	asserts.NoError(t, err, "New")

	replied := make(chan struct{})
	asserts.NoError(t, bot.HandleFunc("^ping$", func(ctx context.Context, b *core.Bot, m *core.Message) {
		asserts.NoError(t, b.SendMessageContext(ctx, m.ChannelID, "pong"), "reply send")
		close(replied)
	}), "HandleFunc")

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- bot.Run(ctx) }()

	daemon := acceptConn(t, conns)
	r := bufio.NewReader(daemon)
	writeLine(t, daemon, `{"jsonrpc":"2.0","method":"receive","params":{"account":"+15550001","envelope":{"source":"+15550002","sourceNumber":"+15550002","sourceName":"Ada","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"ping"}}}}`)

	req := readRequest(t, r)
	params := req["params"].(map[string]any)
	asserts.Equal(t, params["message"], "pong", "handler reply should reach the daemon")
	id := int(req["id"].(float64))
	writeLine(t, daemon, `{"jsonrpc":"2.0","id":`+strconv.Itoa(id)+`,"result":{}}`)

	select {
	case <-replied:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never completed its reply")
	}

	cancel()
	select {
	case err := <-runErr:
		asserts.NoError(t, err, "Run should return nil on a clean ctx cancel")
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
