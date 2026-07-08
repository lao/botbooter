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

// nonDiscordAdapter is a minimal core.Adapter that is not discord's *adapter.
type nonDiscordAdapter struct{}

func (nonDiscordAdapter) Connect(context.Context, core.AdapterDeps) error              { return nil }
func (nonDiscordAdapter) Disconnect() error                                            { return nil }
func (nonDiscordAdapter) Send(context.Context, string, string, core.SendOptions) error { return nil }
func (nonDiscordAdapter) Attachments(*core.Message) ([]core.Attachment, error)         { return nil, nil }

func TestSend(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		a := newTestAdapter(t)
		a.session.Client = &http.Client{Transport: stubRoundTripper{status: 200, body: "{}"}}

		asserts.NoError(t, a.Send(context.Background(), "C1", "hi", core.SendOptions{}), "Send should succeed on 200")
	})

	t.Run("Error", func(t *testing.T) {
		a := newTestAdapter(t)
		a.session.Client = &http.Client{
			Transport: stubRoundTripper{status: 401, body: `{"code":0,"message":"401: Unauthorized"}`},
		}

		asserts.Error(t, a.Send(context.Background(), "C1", "hi", core.SendOptions{}), "Send should fail on 401")
	})
}

// capturingRoundTripper records the last request body so a test can assert the
// reply reference discordgo posted.
type capturingRoundTripper struct {
	body string
}

func (c *capturingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		c.body = string(b)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
}

func TestSend_Threading(t *testing.T) {
	t.Run("InReplyToReferencesMessageID", func(t *testing.T) {
		a := newTestAdapter(t)
		rt := &capturingRoundTripper{}
		a.session.Client = &http.Client{Transport: rt}

		err := a.Send(context.Background(), "C1", "hi",
			core.SendOptions{ReplyTo: &core.Message{ChannelID: "C1", ID: "M9"}})

		asserts.NoError(t, err, "Send")
		asserts.True(t, strings.Contains(rt.body, `"message_reference"`), "reply carries a message_reference")
		asserts.True(t, strings.Contains(rt.body, `"message_id":"M9"`), "reference points at the message ID")
	})

	t.Run("WithThreadIDReferencesRawID", func(t *testing.T) {
		a := newTestAdapter(t)
		rt := &capturingRoundTripper{}
		a.session.Client = &http.Client{Transport: rt}

		err := a.Send(context.Background(), "C1", "hi", core.SendOptions{ThreadID: "RAW7"})

		asserts.NoError(t, err, "Send")
		asserts.True(t, strings.Contains(rt.body, `"message_id":"RAW7"`), "reference points at the raw ThreadID")
	})

	t.Run("NoAnchorSendsPlain", func(t *testing.T) {
		a := newTestAdapter(t)
		rt := &capturingRoundTripper{}
		a.session.Client = &http.Client{Transport: rt}

		err := a.Send(context.Background(), "C1", "hi", core.SendOptions{})

		asserts.NoError(t, err, "Send with no anchor")
		asserts.True(t, !strings.Contains(rt.body, "message_reference"), "no reference when there is no anchor")
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
