package discord

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

type stubRoundTripper struct {
	status int
	body   string
}

func (rt stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// The RoundTripper contract requires closing the request body.
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return &http.Response{
		StatusCode: rt.status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(rt.body)),
	}, nil
}

// capturingRoundTripper records the outgoing request body so a test can assert
// what discordgo serialized.
type capturingRoundTripper struct {
	status int
	body   string
}

func (rt *capturingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		rt.body = string(b)
		_ = req.Body.Close()
	}
	return &http.Response{StatusCode: rt.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
}

func TestSendThreaded(t *testing.T) {
	a := newTestAdapter(t)
	rt := &capturingRoundTripper{status: 200}
	a.session.Client = &http.Client{Transport: rt}

	err := a.SendThreaded(context.Background(), "C1", "M1", "hi")

	asserts.NoError(t, err, "SendThreaded should succeed on 200")
	asserts.True(t, strings.Contains(rt.body, "message_reference"), "body carries a message reference: "+rt.body)
	asserts.True(t, strings.Contains(rt.body, "M1"), "reference targets the reacted message: "+rt.body)
}

func TestSendThreaded_Error(t *testing.T) {
	a := newTestAdapter(t)
	a.session.Client = &http.Client{
		Transport: stubRoundTripper{status: 401, body: `{"code":0,"message":"401: Unauthorized"}`},
	}

	asserts.Error(t, a.SendThreaded(context.Background(), "C1", "M1", "hi"), "SendThreaded should fail on 401")
}

// nonDiscordAdapter is a minimal core.Adapter that is not discord's *adapter.
type nonDiscordAdapter struct{}

func (nonDiscordAdapter) Connect(context.Context, core.AdapterDeps) error      { return nil }
func (nonDiscordAdapter) Disconnect() error                                    { return nil }
func (nonDiscordAdapter) Send(context.Context, string, string) error           { return nil }
func (nonDiscordAdapter) Attachments(*core.Message) ([]core.Attachment, error) { return nil, nil }

func TestSend(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		a := newTestAdapter(t)
		a.session.Client = &http.Client{Transport: stubRoundTripper{status: 200, body: "{}"}}

		asserts.NoError(t, a.Send(context.Background(), "C1", "hi"), "Send should succeed on 200")
	})

	t.Run("Error", func(t *testing.T) {
		a := newTestAdapter(t)
		a.session.Client = &http.Client{
			Transport: stubRoundTripper{status: 401, body: `{"code":0,"message":"401: Unauthorized"}`},
		}

		asserts.Error(t, a.Send(context.Background(), "C1", "hi"), "Send should fail on 401")
	})
}

func TestAttachments(t *testing.T) {
	a := newTestAdapter(t)

	t.Run("DiscordRaw", func(t *testing.T) {
		m := &core.Message{Raw: &discordgo.MessageCreate{Message: &discordgo.Message{
			Attachments: []*discordgo.MessageAttachment{
				{URL: "https://example.com/image.png", Width: 100, Height: 100},
			},
		}}}

		got, err := a.Attachments(m)

		asserts.NoError(t, err, "Attachments")
		asserts.Equal(t, len(got), 1, "one attachment")
		asserts.True(t, got[0].IsImage, "attachment is an image")
		asserts.Equal(t, got[0].URL, "https://example.com/image.png", "attachment URL")
	})

	t.Run("NonDiscordRaw", func(t *testing.T) {
		got, err := a.Attachments(&core.Message{Raw: "not a discord event"})

		asserts.NoError(t, err, "Attachments")
		asserts.True(t, got == nil, "non-Discord message yields no attachments")
	})
}

func TestSession_NonDiscordBot(t *testing.T) {
	bot := core.New(core.SlackBotType, nonDiscordAdapter{})

	asserts.True(t, Session(bot) == nil, "Session returns nil for a non-Discord bot")
}
