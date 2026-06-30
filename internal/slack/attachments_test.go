package slack

import (
	"context"

	"testing"

	"github.com/slack-go/slack/slackevents"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

func TestAttachments(t *testing.T) {
	a := newTestAdapter()

	m := &core.Message{Raw: &slackevents.MessageEvent{
		Files: []slackevents.File{{Mimetype: "image/png", URLPrivate: "https://example.com/image.png"}},
	}}

	atts, err := a.Attachments(m)

	asserts.NoError(t, err, "Attachments should not fail")
	asserts.Equal(t, len(atts), 1, "one attachment")
	asserts.True(t, atts[0].IsImage, "attachment is an image")
	asserts.Equal(t, atts[0].URL, "https://example.com/image.png", "URL is the url_private")
}

func TestAttachments_NonSlackRaw(t *testing.T) {
	a := newTestAdapter()

	atts, err := a.Attachments(&core.Message{Raw: "not a slack event"})

	asserts.NoError(t, err, "non-Slack Raw should not fail")
	asserts.Equal(t, len(atts), 0, "non-Slack Raw yields no attachments")
}

// nonSlackAdapter is a minimal core.Adapter that is not slack's *adapter, used to
// exercise the nil branches of the package accessors.
type nonSlackAdapter struct{}

func (nonSlackAdapter) Connect(context.Context, core.AdapterDeps) error      { return nil }
func (nonSlackAdapter) Disconnect() error                                    { return nil }
func (nonSlackAdapter) Send(context.Context, string, string) error           { return nil }
func (nonSlackAdapter) Attachments(*core.Message) ([]core.Attachment, error) { return nil, nil }

func TestClientAccessors_NonSlackBot(t *testing.T) {
	bot := core.New(core.DiscordBotType, nonSlackAdapter{})

	asserts.True(t, Client(bot) == nil, "Client is nil for a non-Slack bot")
	asserts.True(t, SocketClient(bot) == nil, "SocketClient is nil for a non-Slack bot")
}
