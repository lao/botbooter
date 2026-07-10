package whatsmeow

import (
	"context"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow/types/events"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/internal/asserts"
)

func TestNew(t *testing.T) {
	bot, err := New(Config{DBPath: filepath.Join(t.TempDir(), "wa.db")})
	asserts.NoError(t, err, "new whatsmeow bot")
	asserts.Equal(t, bot.BotType, botbooter.WhatsMeowBotType, "bot type")
	asserts.NotNil(t, Client(bot), "client accessor")
}

func TestRawMessage(t *testing.T) {
	ev := &events.Message{}
	got, ok := RawMessage(&botbooter.Message{Raw: ev})
	asserts.True(t, ok, "whatsmeow raw message recovered")
	asserts.Equal(t, got, ev, "same event")

	_, ok = RawMessage(&botbooter.Message{Raw: "other"})
	asserts.False(t, ok, "foreign raw payload rejected")
}

func TestDownloadForeignBot(t *testing.T) {
	_, err := Download(context.Background(), &botbooter.Bot{}, botbooter.Attachment{})
	asserts.ErrorIs(t, err, botbooter.ErrUnknownBotType, "foreign bot rejected")
}
