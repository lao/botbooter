package telegram

import (
	"context"
	"testing"

	"github.com/go-telegram/bot/models"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// nonTelegramAdapter is a minimal core.Adapter that is not telegram's *adapter.
type nonTelegramAdapter struct{}

func (nonTelegramAdapter) Connect(context.Context, core.AdapterDeps) error              { return nil }
func (nonTelegramAdapter) Disconnect() error                                            { return nil }
func (nonTelegramAdapter) Send(context.Context, string, string, core.SendOptions) error { return nil }
func (nonTelegramAdapter) Attachments(*core.Message) ([]core.Attachment, error)         { return nil, nil }

func TestAttachments(t *testing.T) {
	a := &adapter{}

	got, err := a.Attachments(&core.Message{Raw: &models.Update{Message: &models.Message{
		Photo: []models.PhotoSize{{FileID: "f", Width: 800, Height: 800}},
	}}})
	asserts.NoError(t, err, "attachments from a telegram update")
	asserts.Equal(t, len(got), 1, "one photo attachment")
	asserts.True(t, got[0].IsImage, "photo is an image")

	none, err := a.Attachments(&core.Message{})
	asserts.NoError(t, err, "non-telegram message")
	asserts.Equal(t, len(none), 0, "no telegram raw yields no attachments")
}

func TestClient_NonTelegramBot(t *testing.T) {
	bot := core.New(core.SlackBotType, nonTelegramAdapter{})
	asserts.True(t, Client(bot) == nil, "Client is nil for a non-telegram bot")
}
