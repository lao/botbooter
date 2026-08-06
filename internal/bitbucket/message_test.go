package bitbucket

import (
	"testing"
	"time"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

const (
	cloudPRComment = `{
		"actor": {"uuid": "{actor-uuid}", "display_name": "Bot User", "nickname": "bot"},
		"repository": {"full_name": "myws/myrepo", "name": "myrepo"},
		"pullrequest": {"id": 42, "title": "Add feature"},
		"comment": {"id": 1001, "content": {"raw": "please deploy"}, "created_on": "2026-08-03T10:00:00Z"}
	}`

	cloudIssueComment = `{
		"actor": {"uuid": "{issue-actor}", "display_name": "Reporter"},
		"repository": {"full_name": "myws/myrepo"},
		"issue": {"id": 7, "title": "Bug"},
		"comment": {"id": 2002, "content": {"raw": "still broken"}, "created_on": "2026-08-03T11:30:00Z"}
	}`

	serverPRComment = `{
		"actor": {"id": 5, "name": "botuser", "slug": "botuser", "displayName": "Bot User"},
		"pullRequest": {"id": 42, "title": "Add feature", "toRef": {"repository": {"slug": "myrepo", "project": {"key": "PROJ"}}}},
		"comment": {"id": 3003, "text": "deploy please", "createdDate": 1754215200000}
	}`
)

func TestCloudParseComment(t *testing.T) {
	t.Run("PullRequest", func(t *testing.T) {
		msg, ok := (&cloudFlavor{}).parseComment("pullrequest:comment_created", []byte(cloudPRComment))
		asserts.True(t, ok, "parsed")
		asserts.Equal(t, msg.ChannelID, "myws/myrepo!42", "channel id")
		asserts.Equal(t, msg.UserID, "{actor-uuid}", "user id keys on uuid")
		asserts.Equal(t, msg.AuthorName, "Bot User", "author name")
		asserts.Equal(t, msg.Content, "please deploy", "content")
		asserts.Equal(t, msg.ID, "1001", "comment id")
		asserts.Equal(t, msg.Timestamp.Equal(time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)), true, "timestamp")
		raw, isBB := RawEvent(msg)
		asserts.True(t, isBB, "raw event present")
		asserts.NotNil(t, raw.CloudPRComment, "cloud PR comment union set")
		asserts.True(t, raw.CloudIssueComment == nil && raw.ServerPRComment == nil, "only one union field set")
	})

	t.Run("Issue", func(t *testing.T) {
		msg, ok := (&cloudFlavor{}).parseComment("issue:comment_created", []byte(cloudIssueComment))
		asserts.True(t, ok, "parsed")
		asserts.Equal(t, msg.ChannelID, "myws/myrepo#7", "issue channel id uses #")
		asserts.Equal(t, msg.UserID, "{issue-actor}", "user id")
		asserts.Equal(t, msg.Content, "still broken", "content")
		asserts.Equal(t, msg.ID, "2002", "comment id")
		raw, _ := RawEvent(msg)
		asserts.NotNil(t, raw.CloudIssueComment, "cloud issue comment union set")
	})

	t.Run("FractionalTimestamp", func(t *testing.T) {
		// Bitbucket Cloud can send fractional seconds with an explicit numeric
		// offset; parseRFC3339 must preserve the instant and sub-second precision,
		// not fall back to the zero time.
		payload := `{
			"actor": {"uuid": "{actor-uuid}"},
			"repository": {"full_name": "myws/myrepo"},
			"pullrequest": {"id": 42},
			"comment": {"id": 1001, "content": {"raw": "x"}, "created_on": "2026-08-03T10:00:00.123456+00:00"}
		}`
		msg, ok := (&cloudFlavor{}).parseComment("pullrequest:comment_created", []byte(payload))
		asserts.True(t, ok, "parsed")
		want := time.Date(2026, 8, 3, 10, 0, 0, 123456000, time.UTC)
		asserts.True(t, msg.Timestamp.Equal(want), "fractional-second timestamp preserved")
		asserts.Equal(t, msg.Timestamp.Nanosecond(), 123456000, "sub-second precision preserved")
	})

	t.Run("Unparseable", func(t *testing.T) {
		_, ok := (&cloudFlavor{}).parseComment("pullrequest:comment_created", []byte(`{not json`))
		asserts.False(t, ok, "unparseable body rejected")
	})
}

func TestServerParseComment(t *testing.T) {
	msg, ok := (&serverFlavor{}).parseComment("pr:comment:added", []byte(serverPRComment))
	asserts.True(t, ok, "parsed")
	asserts.Equal(t, msg.ChannelID, "PROJ/myrepo!42", "DC channel id is projectkey/repo!id")
	asserts.Equal(t, msg.UserID, "botuser", "user id keys on slug")
	asserts.Equal(t, msg.AuthorName, "Bot User", "author name")
	asserts.Equal(t, msg.Content, "deploy please", "content from text key")
	asserts.Equal(t, msg.ID, "3003", "comment id")
	asserts.Equal(t, msg.Timestamp.Equal(time.UnixMilli(1754215200000).UTC()), true, "timestamp from millis")
	raw, _ := RawEvent(msg)
	asserts.NotNil(t, raw.ServerPRComment, "server PR comment union set")
	asserts.True(t, raw.CloudPRComment == nil && raw.CloudIssueComment == nil, "only one union field set")
}

func TestCategory(t *testing.T) {
	cloud := &cloudFlavor{}
	asserts.Equal(t, cloud.category("pullrequest:comment_created"), catComment, "cloud PR comment")
	asserts.Equal(t, cloud.category("issue:comment_created"), catComment, "cloud issue comment")
	asserts.Equal(t, cloud.category("pullrequest:created"), catPullRequest, "cloud PR created")
	asserts.Equal(t, cloud.category("pullrequest:updated"), catPullRequest, "cloud PR updated")
	asserts.Equal(t, cloud.category("repo:push"), catPush, "cloud push")
	// An edit arrives under a key we do not route: ignored.
	asserts.Equal(t, cloud.category("pullrequest:comment_updated"), catUnknown, "cloud comment edit ignored")
	asserts.Equal(t, cloud.category("pullrequest:approved"), catUnknown, "non-reviewable PR action ignored")

	server := &serverFlavor{}
	asserts.Equal(t, server.category("pr:comment:added"), catComment, "DC PR comment")
	asserts.Equal(t, server.category("pr:opened"), catPullRequest, "DC PR opened")
	asserts.Equal(t, server.category("pr:from_ref_updated"), catPullRequest, "DC PR updated")
	asserts.Equal(t, server.category("repo:refs_changed"), catPush, "DC push")
	asserts.Equal(t, server.category("pr:comment:edited"), catUnknown, "DC comment edit ignored")
}

// RawEvent must reject a foreign payload.
func TestRawEventForeign(t *testing.T) {
	_, ok := RawEvent(&core.Message{Raw: "not a bitbucket event"})
	asserts.False(t, ok, "foreign raw payload rejected")
}
