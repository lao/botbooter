// Package telegram exposes the Telegram (Bot API) constructor and the raw-update
// and client accessors for botbooter. Import it for a Telegram bot; a
// Telegram-only binary pulls in go-telegram but never discordgo or slack-go.
package telegram

import (
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/lao/botbooter"
	tgint "github.com/lao/botbooter/internal/telegram"
)

// New creates a Telegram bot from a BotFather token. It returns an error only
// if go-telegram rejects the token at construction; an invalid token otherwise
// surfaces when Run or Connect starts polling.
func New(token string) (*botbooter.Bot, error) {
	return tgint.New(token)
}

// RawUpdate returns the raw Telegram update carried on m, reporting whether m
// originated from Telegram.
func RawUpdate(m *botbooter.Message) (*models.Update, bool) {
	return tgint.RawUpdate(m)
}

// Client returns the go-telegram bot client backing b, or nil if b is not a
// Telegram bot.
func Client(b *botbooter.Bot) *bot.Bot {
	return tgint.Client(b)
}

// EnvSuppressURLWarning names the environment variable that silences the
// plaintext-token warning the Telegram resolver logs on every successful
// attachment resolve. Set it to any non-empty value to opt out.
const EnvSuppressURLWarning = tgint.EnvSuppressURLWarning
