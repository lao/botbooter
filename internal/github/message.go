package github

import (
	"strconv"

	gogithub "github.com/google/go-github/v88/github"
	"github.com/lao/botbooter/internal/core"
)

// Message is the typed raw payload stored in core.Message.Raw for GitHub bots.
// Consumers can distinguish PR comments from issue comments via
// Event.GetIssue().IsPullRequest().
type Message struct {
	Event *gogithub.IssueCommentEvent
}

// RawEvent returns the typed issue_comment event carried on m, reporting
// whether m originated from GitHub.
func RawEvent(m *core.Message) (*Message, bool) {
	v, ok := m.Raw.(*Message)
	return v, ok
}

// toMessage maps an issue_comment event into the platform-agnostic message.
// ChannelID is "owner/repo#number", exactly what Send's parseChannelID accepts,
// so bot.Send(ctx, msg.ChannelID, ...) replies on the same issue or PR.
func toMessage(event *gogithub.IssueCommentEvent) *core.Message {
	comment := event.GetComment()
	return &core.Message{
		ID:         strconv.FormatInt(comment.GetID(), 10),
		UserID:     strconv.FormatInt(comment.GetUser().GetID(), 10),
		AuthorName: comment.GetUser().GetLogin(),
		ChannelID:  event.GetRepo().GetFullName() + "#" + strconv.Itoa(event.GetIssue().GetNumber()),
		Content:    comment.GetBody(),
		Timestamp:  comment.GetCreatedAt().Time,
		Raw:        &Message{Event: event},
	}
}
