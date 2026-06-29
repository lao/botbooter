package main

import (
	"cmp"
	"context"
	"log"

	"github.com/lao/botbooter"
)

func loggingMiddleware(ctx context.Context, bot *botbooter.Bot, message *botbooter.Message, next botbooter.CommandHandler) {
	// AuthorName is best-effort; empty on platforms that deliver only an id (e.g. Slack).
	log.Printf("message from %s in channel %s:", cmp.Or(message.AuthorName, message.UserID), message.ChannelID)
	log.Printf("  ID:         %s", message.ID)
	log.Printf("  UserID:     %s", message.UserID)
	log.Printf("  AuthorName: %s", message.AuthorName)
	log.Printf("  ChannelID:  %s", message.ChannelID)
	log.Printf("  Content:    %s", message.Content)
	log.Printf("  Timestamp:  %s", message.Timestamp)
	log.Printf("  ReplyToID:  %s", message.ReplyToID)
	log.Printf("  MentionedUserIDs: %v", message.MentionedUserIDs)

	// bot.ResolveAttachmentURL(ctx, a) is the unified way to get a downloadable
	// URL: Telegram resolves a link that EMBEDS the bot token in plaintext —
	// secret, so this demo does not print it.
	if attachments, err := bot.GetAttachments(message); err != nil {
		log.Println("  failed to get attachments:", err)
	} else {
		for i, attachment := range attachments {
			log.Printf("  attachment[%d]: isImage=%t url=%q extraData=%+v", i, attachment.IsImage, attachment.URL, attachment.ExtraData)
			if url, err := bot.ResolveAttachmentURL(ctx, attachment); err != nil {
				log.Printf("  attachment[%d]: resolve failed: %v", i, err)
			} else if url != "" {
				log.Printf("  attachment[%d]: resolved a downloadable URL (not printed; may embed a secret)", i)
			}
		}
	}

	next(ctx, bot, message)
}
