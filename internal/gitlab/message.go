package gitlab

import (
	"strconv"
	"time"

	gogitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/lao/botbooter/internal/core"
)

// Message is the typed raw payload stored in core.Message.Raw for GitLab bots.
// A Note Hook resolves to exactly one comment kind, so exactly one field is
// non-nil: IssueComment for a comment on an issue, MergeComment for a comment on
// a merge request. Branch on whichever is set to reach the platform's untouched
// event.
type Message struct {
	IssueComment *gogitlab.IssueCommentEvent
	MergeComment *gogitlab.MergeCommentEvent
}

// RawEvent returns the typed note payload carried on m, reporting whether m
// originated from GitLab.
func RawEvent(m *core.Message) (*Message, bool) {
	v, ok := m.Raw.(*Message)
	return v, ok
}

// toMessageFromIssueComment maps an issue-comment Note event into the
// platform-agnostic message. ChannelID is "group/project#iid", exactly what
// Send's parseChannelID accepts, so bot.Send(ctx, msg.ChannelID, ...) replies on
// the same issue.
func toMessageFromIssueComment(event *gogitlab.IssueCommentEvent) *core.Message {
	var userID int64
	var username string
	if event.User != nil {
		userID, username = event.User.ID, event.User.Username
	}
	return newMessage(
		event.ObjectAttributes.ID,
		userID, username,
		event.Project.PathWithNamespace+"#"+strconv.FormatInt(event.Issue.IID, 10),
		event.ObjectAttributes.Note,
		event.ObjectAttributes.CreatedAt,
		&Message{IssueComment: event},
	)
}

// toMessageFromMergeComment maps a merge-request-comment Note event into the
// platform-agnostic message. ChannelID is "group/project!iid" (GitLab's native
// merge-request notation), which Send routes to the merge-request note endpoint.
func toMessageFromMergeComment(event *gogitlab.MergeCommentEvent) *core.Message {
	var userID int64
	var username string
	if event.User != nil {
		userID, username = event.User.ID, event.User.Username
	}
	return newMessage(
		event.ObjectAttributes.ID,
		userID, username,
		event.Project.PathWithNamespace+"!"+strconv.FormatInt(event.MergeRequest.IID, 10),
		event.ObjectAttributes.Note,
		event.ObjectAttributes.CreatedAt,
		&Message{MergeComment: event},
	)
}

// newMessage folds the fields common to both note kinds into a core.Message,
// so the two mappers differ only in how they extract the noteable's channel id
// and which typed event they carry.
func newMessage(id, userID int64, username, channelID, content, createdAt string, raw *Message) *core.Message {
	return &core.Message{
		ID:         strconv.FormatInt(id, 10),
		UserID:     strconv.FormatInt(userID, 10),
		AuthorName: username,
		ChannelID:  channelID,
		Content:    content,
		Timestamp:  parseTimestamp(createdAt),
		Raw:        raw,
	}
}

// parseTimestamp best-effort-parses a GitLab webhook note timestamp. GitLab has
// emitted these in more than one shape across versions (ISO-8601 and a
// space-separated "... UTC"/"... -0700" form), and Timestamp is best-effort per
// core.Message's contract, so an unrecognized value yields the zero time rather
// than an error.
func parseTimestamp(s string) time.Time {
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02 15:04:05 MST",
		"2006-01-02 15:04:05 -0700",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
