// Package telegram exposes the Telegram (Bot API) constructor and the
// raw-update, client and attachment-URL helpers for botbooter. Import it for a
// Telegram bot; a Telegram-only binary pulls in go-telegram but never discordgo
// or slack-go.
package telegram

import (
	"context"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/lao/botbooter"
	tgint "github.com/lao/botbooter/internal/telegram"
)

// New creates a Telegram bot from a BotFather token.
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

// ErrNotTelegramBot is returned by [ResolveAttachmentURL] when b is not a
// Telegram bot.
var ErrNotTelegramBot = tgint.ErrNotTelegramBot

// EnvSuppressURLWarning names the environment variable that silences the
// plaintext-token warning [ResolveAttachmentURL] logs on every successful
// resolve. Set it to any non-empty value to opt out.
const EnvSuppressURLWarning = tgint.EnvSuppressURLWarning

// ResolveAttachmentURL fetches a downloadable URL for a Telegram attachment via
// the Bot API getFile method. It returns [ErrNotTelegramBot] if b is not a
// Telegram bot, and ("", nil) if att carries no Telegram file id. The returned
// URL embeds the bot token in plaintext — treat it as secret and do not log it.
func ResolveAttachmentURL(ctx context.Context, b *botbooter.Bot, att botbooter.Attachment) (string, error) {
	return tgint.ResolveAttachmentURL(ctx, b, att)
}
