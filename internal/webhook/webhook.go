// Package webhook is an HTTP webhook adapter for botbooter. It runs an HTTP
// server that turns inbound POST requests into messages and dispatches them,
// implementing core.Adapter.
//
// Unlike the socket and long-poll adapters, which hold one persistent connection
// per bot identity, a webhook bot is just an HTTP handler: run several instances
// behind a load balancer to absorb high inbound volume, and pair it with
// (*core.Bot).WithWorkers so accepted requests are processed off the request
// goroutine and acked immediately.
package webhook

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/lao/botbooter/internal/core"
)

// ErrNoSender is returned by Send when the webhook bot was created without a
// Sender. The webhook transport is inbound-only, so replies need an explicit
// outbound path (typically a platform Web API call).
var ErrNoSender = errors.New("botbooter: webhook adapter has no sender configured")

const (
	defaultAddr       = ":8080"
	defaultPath       = "/"
	maxBodyBytes      = 1 << 20 // 1 MiB cap on a decoded request body.
	shutdownTimeout   = 5 * time.Second
	readHeaderTimeout = 10 * time.Second
)

// Payload is the default JSON body decoded from a request when no custom Decoder
// is configured.
type Payload struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	ChannelID string `json:"channel_id"`
	Content   string `json:"content"`
}

// Decoder turns an inbound HTTP request into a platform-agnostic message. It is
// called once per accepted POST. Returning an error rejects the request with
// 400; returning a nil message accepts it with 200 but dispatches nothing
// (useful for verification handshakes or events the bot ignores).
type Decoder func(r *http.Request) (*core.Message, error)

// Sender delivers an outbound reply for SendMessage. The webhook transport has
// no implicit reply channel, so a bot that responds supplies one.
type Sender func(ctx context.Context, channelID, text string) error

// Config configures a webhook bot. The zero value is usable: it listens on
// :8080, routes every path, decodes the default JSON Payload, and has no Sender.
type Config struct {
	Addr     string
	Path     string
	Decoder  Decoder
	Sender   Sender
	Listener net.Listener
}

type adapter struct {
	addr     string
	path     string
	decoder  Decoder
	sender   Sender
	listener net.Listener

	mu     sync.Mutex
	server *http.Server
}

func newAdapter(cfg Config) *adapter {
	a := &adapter{
		addr:     cmp.Or(cfg.Addr, defaultAddr),
		path:     cmp.Or(cfg.Path, defaultPath),
		decoder:  cfg.Decoder,
		sender:   cfg.Sender,
		listener: cfg.Listener,
	}
	if a.decoder == nil {
		a.decoder = decodePayload
	}
	return a
}

// New creates a webhook bot from cfg.
func New(cfg Config) *core.Bot {
	return core.New(core.WebhookBotType, newAdapter(cfg))
}

// Connect binds the listener and serves the HTTP handler in the background; it
// returns once the listener is open, so a bind error (e.g. address in use)
// surfaces synchronously like the other adapters' connect errors.
func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	ln := a.listener
	if ln == nil {
		var err error
		ln, err = net.Listen("tcp", a.addr)
		if err != nil {
			return err
		}
	}

	mux := http.NewServeMux()
	mux.Handle(a.path, a.handler(ctx, deps))
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	a.mu.Lock()
	a.server = srv
	a.mu.Unlock()

	// Cancellation tears the server down even when the bot is driven via Connect
	// rather than Run (which would call Disconnect itself).
	go func() {
		<-ctx.Done()
		_ = deps.Disconnect()
	}()

	go func() {
		err := srv.Serve(ln)
		// A graceful Shutdown returns ErrServerClosed; report the context error
		// instead so Run can swallow a clean shutdown like the other adapters.
		if errors.Is(err, http.ErrServerClosed) {
			err = ctx.Err()
		}
		deps.Done(err)
	}()

	return nil
}

// handler builds the HTTP handler. It dispatches with ctx (the run context), not
// the request context: the handler acks and returns at once, which cancels the
// request context and would kill any dispatch still queued on the worker pool.
func (a *adapter) handler(ctx context.Context, deps core.AdapterDeps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		msg, err := a.decoder(r)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if msg != nil {
			deps.Dispatch(ctx, msg)
		}
		w.WriteHeader(http.StatusOK)
	})
}

// Disconnect gracefully shuts the HTTP server down. It is safe to call when not
// serving.
func (a *adapter) Disconnect() error {
	a.mu.Lock()
	srv := a.server
	a.server = nil
	a.mu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return srv.Shutdown(ctx)
}

// Send delivers text via the configured Sender, or returns ErrNoSender if none
// was set.
func (a *adapter) Send(ctx context.Context, channelID, text string) error {
	if a.sender == nil {
		return ErrNoSender
	}
	return a.sender(ctx, channelID, text)
}

// Attachments returns no attachments: the default payload carries none, and a
// custom Decoder is free to attach platform data via the message's Raw field.
func (a *adapter) Attachments(*core.Message) ([]core.Attachment, error) {
	return nil, nil
}

// decodePayload is the default Decoder: it reads a JSON Payload (bounded in size)
// from the request body and maps it onto a Message.
func decodePayload(r *http.Request) (*core.Message, error) {
	var p Payload
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&p); err != nil {
		return nil, err
	}
	return &core.Message{
		ID:        p.ID,
		UserID:    p.UserID,
		ChannelID: p.ChannelID,
		Content:   p.Content,
		Raw:       &p,
	}, nil
}

// RawPayload returns the default-decoded payload carried on m, reporting whether
// m was produced by the default decoder. A custom Decoder sets its own Raw.
func RawPayload(m *core.Message) (*Payload, bool) {
	p, ok := m.Raw.(*Payload)
	return p, ok
}
