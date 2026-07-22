package github

import (
	"encoding/json"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v88/github"
	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// issueCommentCreated is a trimmed real-shaped issue_comment payload.
const issueCommentCreated = `{
  "action": "created",
  "issue": {"number": 42},
  "comment": {
    "id": 1234567890,
    "body": "/deploy staging",
    "created_at": "2026-07-03T10:00:00Z",
    "user": {"id": 58394, "login": "octocat", "type": "User"}
  },
  "repository": {"full_name": "lao/botbooter"},
  "sender": {"id": 58394, "login": "octocat", "type": "User"}
}`

func parseEvent(t *testing.T, payload string) *gogithub.IssueCommentEvent {
	t.Helper()
	var ev gogithub.IssueCommentEvent
	asserts.NoError(t, json.Unmarshal([]byte(payload), &ev), "parse test payload")
	return &ev
}

func TestToMessage(t *testing.T) {
	got := toMessage(parseEvent(t, issueCommentCreated))

	asserts.Equal(t, got.ID, "1234567890", "ID is the comment id")
	asserts.Equal(t, got.UserID, "58394", "UserID is the author's numeric id")
	asserts.Equal(t, got.AuthorName, "octocat", "AuthorName is the login")
	asserts.Equal(t, got.ChannelID, "lao/botbooter#42", "ChannelID round-trips into Send")
	asserts.Equal(t, got.Content, "/deploy staging", "Content is the comment body")
	asserts.True(t, got.Timestamp.Equal(time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)), "Timestamp from created_at")
}

func TestRawEvent(t *testing.T) {
	ev := parseEvent(t, issueCommentCreated)
	m := toMessage(ev)

	raw, ok := RawEvent(m)
	asserts.True(t, ok, "raw event present")
	asserts.True(t, raw.Event == ev, "raw carries the original event")

	_, ok = RawEvent(&core.Message{Raw: "not ours"})
	asserts.False(t, ok, "foreign raw payload reports false")
}
