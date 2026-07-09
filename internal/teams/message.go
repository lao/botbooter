// Inbound Activity parsing: the public Message view, the Bot Framework wire types,
// and the mapping into core.Message.

package teams

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/lao/botbooter/internal/core"
)

// Message is the parsed payload of an inbound Bot Framework Activity. Raw holds
// the original Activity JSON for callers that need fields beyond these.
type Message struct {
	ID             string
	ConversationID string
	ServiceURL     string
	From           string
	AuthorName     string
	Text           string
	Timestamp      time.Time
	Raw            json.RawMessage

	// attachments is decoded once at parse time so Attachments need not re-unmarshal
	// Raw on every lookup. Unexported: callers reach it via Bot.Attachments or Raw.
	attachments []activityAttachment
}

// RawMessage returns the parsed Teams message carried on m, reporting whether m
// originated from Teams.
func RawMessage(m *core.Message) (*Message, bool) {
	tm, ok := m.Raw.(*Message)
	return tm, ok
}

type channelAccount struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Role is the account's RoleType. Teams sets from.role to "bot" on bot-authored
	// messages (the bot's own echo or another bot), which is how those are dropped.
	Role string `json:"role"`
}

// mentionEntity is a Bot Framework "mention" entity: Text is the literal markup
// span (e.g. "<at>Bot Name</at>") and Mentioned is who was mentioned.
type mentionEntity struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	Mentioned channelAccount `json:"mentioned"`
}

type inboundActivity struct {
	Type         string         `json:"type"`
	ID           string         `json:"id"`
	Text         string         `json:"text"`
	ServiceURL   string         `json:"serviceUrl"`
	ChannelID    string         `json:"channelId"`
	Timestamp    string         `json:"timestamp"`
	ReplyToID    string         `json:"replyToId"`
	From         channelAccount `json:"from"`
	Recipient    channelAccount `json:"recipient"`
	Conversation struct {
		ID string `json:"id"`
	} `json:"conversation"`
	Attachments []activityAttachment `json:"attachments"`
	Entities    []mentionEntity      `json:"entities"`
}

func toMessage(act *inboundActivity, raw json.RawMessage) *core.Message {
	ts := parseTimestamp(act.Timestamp)
	tm := &Message{
		ID:             act.ID,
		ConversationID: act.Conversation.ID,
		ServiceURL:     act.ServiceURL,
		From:           act.From.ID,
		AuthorName:     act.From.Name,
		Text:           act.Text,
		Timestamp:      ts,
		Raw:            raw,
		attachments:    act.Attachments,
	}
	return &core.Message{
		ID:               act.ID,
		UserID:           act.From.ID,
		AuthorName:       act.From.Name,
		ChannelID:        act.Conversation.ID,
		Content:          stripRecipientMention(act),
		Timestamp:        ts,
		ReplyToID:        act.ReplyToID,
		MentionedUserIDs: mentionedUserIDs(act),
		Raw:              tm,
	}
}

// mentionedUserIDs collects the IDs mentioned in the Activity's entities,
// excluding the bot itself (Recipient) — the same entity stripRecipientMention
// removes from the text.
func mentionedUserIDs(act *inboundActivity) []string {
	var ids []string
	for _, e := range act.Entities {
		if !strings.EqualFold(e.Type, "mention") || e.Mentioned.ID == "" {
			continue
		}
		if e.Mentioned.ID == act.Recipient.ID {
			continue
		}
		ids = append(ids, e.Mentioned.ID)
	}
	return ids
}

// stripRecipientMention removes the bot's own @mention markup from the Activity
// text. Group and channel messages arrive prefixed with markup like
// "<at>Bot Name</at> echo hi", which would stop an anchored pattern (e.g. ^echo)
// from matching. Only the entity targeting Recipient.ID is removed, so mentions of
// other users survive; the internal Message.Text keeps the raw wire text.
func stripRecipientMention(act *inboundActivity) string {
	if act.Recipient.ID == "" {
		return act.Text
	}
	text := act.Text
	for _, e := range act.Entities {
		if strings.EqualFold(e.Type, "mention") && e.Text != "" && e.Mentioned.ID == act.Recipient.ID {
			text = strings.Replace(text, e.Text, "", 1)
		}
	}
	// Unchanged text (no entity matched, or a no-op Replace) passes through untrimmed.
	if text == act.Text {
		return act.Text
	}
	return strings.TrimSpace(text)
}

func parseTimestamp(s string) time.Time {
	// RFC3339Nano is a superset of RFC3339, so it also parses plain RFC3339.
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
