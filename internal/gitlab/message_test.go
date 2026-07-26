package gitlab

import (
	"encoding/json"
	"testing"
	"time"

	gogitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// issueNoteCreated is a trimmed real-shaped Note Hook payload for a comment on
// an issue.
const issueNoteCreated = `{
  "object_kind": "note",
  "event_type": "note",
  "user": {"id": 58, "username": "octocat", "name": "Octo Cat"},
  "project": {"path_with_namespace": "acme/widgets"},
  "object_attributes": {
    "id": 1234,
    "note": "/deploy staging",
    "noteable_type": "Issue",
    "author_id": 58,
    "created_at": "2026-07-03 10:00:00 UTC",
    "system": false,
    "action": "create"
  },
  "issue": {"iid": 17}
}`

// mergeNoteCreated is a trimmed real-shaped Note Hook payload for a comment on a
// merge request.
const mergeNoteCreated = `{
  "object_kind": "note",
  "event_type": "note",
  "user": {"id": 58, "username": "octocat", "name": "Octo Cat"},
  "project": {"path_with_namespace": "acme/widgets"},
  "object_attributes": {
    "id": 5678,
    "note": "/retest",
    "noteable_type": "MergeRequest",
    "author_id": 58,
    "created_at": "2026-07-03 11:00:00 UTC",
    "system": false,
    "action": "create"
  },
  "merge_request": {"iid": 1}
}`

func parseIssueNote(t *testing.T, payload string) *gogitlab.IssueCommentEvent {
	t.Helper()
	var ev gogitlab.IssueCommentEvent
	asserts.NoError(t, json.Unmarshal([]byte(payload), &ev), "parse issue note payload")
	return &ev
}

func parseMergeNote(t *testing.T, payload string) *gogitlab.MergeCommentEvent {
	t.Helper()
	var ev gogitlab.MergeCommentEvent
	asserts.NoError(t, json.Unmarshal([]byte(payload), &ev), "parse merge note payload")
	return &ev
}

func TestToMessageFromIssueComment(t *testing.T) {
	got := toMessageFromIssueComment(parseIssueNote(t, issueNoteCreated))

	asserts.Equal(t, got.ID, "1234", "ID is the note id")
	asserts.Equal(t, got.UserID, "58", "UserID is the author's numeric id")
	asserts.Equal(t, got.AuthorName, "octocat", "AuthorName is the username")
	asserts.Equal(t, got.ChannelID, "acme/widgets#17", "ChannelID round-trips into Send (issue uses #)")
	asserts.Equal(t, got.Content, "/deploy staging", "Content is the note body")
	asserts.True(t, got.Timestamp.Equal(time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)), "Timestamp from created_at")
}

func TestToMessageFromMergeComment(t *testing.T) {
	got := toMessageFromMergeComment(parseMergeNote(t, mergeNoteCreated))

	asserts.Equal(t, got.ID, "5678", "ID is the note id")
	asserts.Equal(t, got.UserID, "58", "UserID is the author's numeric id")
	asserts.Equal(t, got.AuthorName, "octocat", "AuthorName is the username")
	asserts.Equal(t, got.ChannelID, "acme/widgets!1", "ChannelID round-trips into Send (MR uses !)")
	asserts.Equal(t, got.Content, "/retest", "Content is the note body")
}

func TestRawEvent(t *testing.T) {
	issue := parseIssueNote(t, issueNoteCreated)
	m := toMessageFromIssueComment(issue)

	raw, ok := RawEvent(m)
	asserts.True(t, ok, "raw event present")
	asserts.True(t, raw.IssueComment == issue, "raw carries the original issue-comment event")
	asserts.True(t, raw.MergeComment == nil, "merge-comment field is nil for an issue note")

	mergeRaw, _ := RawEvent(toMessageFromMergeComment(parseMergeNote(t, mergeNoteCreated)))
	asserts.True(t, mergeRaw.MergeComment != nil, "merge-comment field set for a merge note")
	asserts.True(t, mergeRaw.IssueComment == nil, "issue-comment field is nil for a merge note")

	_, ok = RawEvent(&core.Message{Raw: "not ours"})
	asserts.False(t, ok, "foreign raw payload reports false")
}

// GitLab has emitted note timestamps in more than one shape across versions;
// parseTimestamp accepts the known ones and degrades an unknown value to the
// zero time rather than erroring (Timestamp is best-effort).
func TestParseTimestamp(t *testing.T) {
	want := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		zero bool
	}{
		{"2026-07-03T10:00:00Z", false},      // RFC3339
		{"2026-07-03 10:00:00 UTC", false},   // space-separated with zone name
		{"2026-07-03 10:00:00 +0000", false}, // space-separated with numeric zone
		{"not a timestamp", true},            // unrecognized → zero
		{"", true},                           // empty → zero
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := parseTimestamp(tc.in)
			if tc.zero {
				asserts.True(t, got.IsZero(), "unrecognized timestamp yields the zero time")
				return
			}
			asserts.True(t, got.Equal(want), "parsed to the expected instant")
		})
	}
}
