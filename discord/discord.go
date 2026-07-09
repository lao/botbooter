// Package discord exposes the Discord (Gateway) constructor and the raw-event
// and session accessors for botbooter. Import it for a Discord bot; a
// Discord-only binary pulls in discordgo but never slack-go or go-telegram.
package discord

import (
	"github.com/bwmarrin/discordgo"

	"github.com/lao/botbooter"
	discordint "github.com/lao/botbooter/internal/discord"
)

// New creates a Discord bot that connects via the Gateway. It returns an error
// only if discordgo rejects the token at construction; an invalid or revoked
// token otherwise surfaces when Run or Connect opens the gateway session.
func New(token string) (*botbooter.Bot, error) {
	return discordint.New(token)
}

// RawEvent returns the raw Discord message-create event carried on m, reporting
// whether m originated from Discord.
func RawEvent(m *botbooter.Message) (*discordgo.MessageCreate, bool) {
	return discordint.RawEvent(m)
}

// Session returns the discordgo gateway session backing b, or nil if b is not a
// Discord bot.
func Session(b *botbooter.Bot) *discordgo.Session {
	return discordint.Session(b)
}
