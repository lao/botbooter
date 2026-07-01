package teams

import (
	"encoding/json"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

func TestParseTimestamp(t *testing.T) {
	got := parseTimestamp("2026-06-30T12:00:00Z")
	asserts.Equal(t, got.IsZero(), false, "valid RFC3339 should parse")
	asserts.Equal(t, parseTimestamp("nonsense").IsZero(), true, "bad timestamp is zero")
	asserts.Equal(t, parseTimestamp("").IsZero(), true, "empty timestamp is zero")
}

func TestToMessage_Mapping(t *testing.T) {
	body := activityJSON("message", "hello", allowedServiceURL, "user-1", "bot-1", "conv-1")
	var act inboundActivity
	asserts.NoError(t, json.Unmarshal([]byte(body), &act), "unmarshal activity")

	m := toMessage(&act, json.RawMessage(body))
	asserts.Equal(t, m.ID, "act-1", "ID")
	asserts.Equal(t, m.UserID, "user-1", "UserID from from.id")
	asserts.Equal(t, m.AuthorName, "Ada", "AuthorName from from.name")
	asserts.Equal(t, m.ChannelID, "conv-1", "ChannelID from conversation.id")
	asserts.Equal(t, m.Content, "hello", "Content from text")

	tm, ok := RawMessage(m)
	asserts.True(t, ok, "RawMessage should report Teams origin")
	asserts.Equal(t, tm.ServiceURL, allowedServiceURL, "ServiceURL carried on raw message")
}

func TestToMessage_ReplyToID(t *testing.T) {
	// A threaded reply carries replyToId; a top-level message omits it.
	withReply := `{"type":"message","id":"act-2","text":"re","replyToId":"act-1"}`
	var act inboundActivity
	asserts.NoError(t, json.Unmarshal([]byte(withReply), &act), "unmarshal reply activity")
	m := toMessage(&act, json.RawMessage(withReply))
	asserts.Equal(t, m.ReplyToID, "act-1", "ReplyToID populated from replyToId")

	var plain inboundActivity
	asserts.NoError(t, json.Unmarshal([]byte(`{"type":"message","id":"act-3"}`), &plain), "unmarshal plain activity")
	asserts.Equal(t, toMessage(&plain, nil).ReplyToID, "", "ReplyToID empty when absent")
}

func TestStripRecipientMention(t *testing.T) {
	const botID = "bot-1"
	mention := func(id, text string) mentionEntity {
		return mentionEntity{Type: "mention", Text: text, Mentioned: channelAccount{ID: id}}
	}
	cases := []struct {
		name     string
		text     string
		entities []mentionEntity
		want     string
	}{
		{"no entities passthrough keeps spacing", "  hello  ", nil, "  hello  "},
		{"bot mention stripped and trimmed", "<at>Bot</at> echo hi", []mentionEntity{mention(botID, "<at>Bot</at>")}, "echo hi"},
		{
			"other user mention preserved", "<at>Bot</at> tell <at>John</at> hi",
			[]mentionEntity{mention(botID, "<at>Bot</at>"), mention("user-2", "<at>John</at>")},
			"tell <at>John</at> hi",
		},
		{"one removal per bot mention entity", "<at>Bot</at> hi <at>Bot</at>", []mentionEntity{mention(botID, "<at>Bot</at>")}, "hi <at>Bot</at>"},
		{"id match but markup absent keeps spacing", "  hello  ", []mentionEntity{mention(botID, "<at>Bot</at>")}, "  hello  "},
		{"non-mention entity ignored", "hi", []mentionEntity{{Type: "clientInfo", Text: "x", Mentioned: channelAccount{ID: botID}}}, "hi"},
		{"empty entity text ignored", "hi", []mentionEntity{mention(botID, "")}, "hi"},
	}
	for _, tc := range cases {
		act := &inboundActivity{Text: tc.text, Recipient: channelAccount{ID: botID}, Entities: tc.entities}
		asserts.Equal(t, stripRecipientMention(act), tc.want, tc.name)
	}

	// An empty recipient id must never match an entity carrying an empty mentioned id.
	emptyRecip := &inboundActivity{Text: "hi", Entities: []mentionEntity{mention("", "<at>x</at>")}}
	asserts.Equal(t, stripRecipientMention(emptyRecip), "hi", "empty recipient id: no strip")
}

func TestToMessage_StripsBotMention(t *testing.T) {
	body := `{"type":"message","id":"a","text":"<at>Bot</at> echo hi","recipient":{"id":"bot-1"},` +
		`"entities":[{"type":"mention","text":"<at>Bot</at>","mentioned":{"id":"bot-1"}}]}`
	var act inboundActivity
	asserts.NoError(t, json.Unmarshal([]byte(body), &act), "unmarshal")
	m := toMessage(&act, json.RawMessage(body))
	asserts.Equal(t, m.Content, "echo hi", "Content strips the bot mention so anchored handlers match")
	tm, ok := RawMessage(m)
	asserts.True(t, ok, "raw message present")
	asserts.Equal(t, tm.Text, "<at>Bot</at> echo hi", "raw Text keeps the original mention markup")
}
