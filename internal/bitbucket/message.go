package bitbucket

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/lao/botbooter/internal/core"
)

// Message is the typed raw payload stored in core.Message.Raw for Bitbucket bots.
// The flavor is fixed at New, so exactly one field is ever non-nil: a Cloud bot
// carries CloudPRComment or CloudIssueComment, a Data Center bot ServerPRComment.
// Branch on whichever is set to reach the platform's untouched event.
type Message struct {
	CloudPRComment    *CloudPullRequestCommentEvent
	CloudIssueComment *CloudIssueCommentEvent
	ServerPRComment   *ServerPullRequestCommentEvent
}

// RawEvent returns the typed payload carried on m, reporting whether m originated
// from Bitbucket.
func RawEvent(m *core.Message) (*Message, bool) {
	v, ok := m.Raw.(*Message)
	return v, ok
}

// --- Cloud (REST 2.0 webhook) payloads ------------------------------------

// CloudActor is the Cloud webhook actor. Identity keys on UUID: account_id is
// frequently absent from Cloud webhook payloads (an Atlassian GDPR change), so
// the reply-loop guard must never depend on it.
type CloudActor struct {
	UUID        string `json:"uuid"`
	AccountID   string `json:"account_id"`
	Nickname    string `json:"nickname"`
	DisplayName string `json:"display_name"`
}

// CloudRepository is the Cloud webhook repository. FullName is "workspace/repo",
// exactly the prefix Send's parseChannelID accepts.
type CloudRepository struct {
	FullName string `json:"full_name"`
	Name     string `json:"name"`
}

// CloudContent is a Cloud comment/description body; Raw is the markdown source.
type CloudContent struct {
	Raw string `json:"raw"`
}

// CloudCommentRef is the parent link of a threaded Cloud comment.
type CloudCommentRef struct {
	ID int64 `json:"id"`
}

// CloudComment is a Cloud pull-request or issue comment.
type CloudComment struct {
	ID        int64            `json:"id"`
	Content   CloudContent     `json:"content"`
	CreatedOn string           `json:"created_on"`
	Parent    *CloudCommentRef `json:"parent"`
}

// CloudPullRequest is the minimal Cloud pull request carried on comment and PR
// events.
type CloudPullRequest struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	State string `json:"state"`
}

// CloudIssue is the minimal Cloud issue carried on issue-comment events.
type CloudIssue struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

// CloudPullRequestCommentEvent is a pullrequest:comment_created delivery.
type CloudPullRequestCommentEvent struct {
	Actor       CloudActor       `json:"actor"`
	Repository  CloudRepository  `json:"repository"`
	PullRequest CloudPullRequest `json:"pullrequest"`
	Comment     CloudComment     `json:"comment"`
}

// CloudIssueCommentEvent is an issue:comment_created delivery.
type CloudIssueCommentEvent struct {
	Actor      CloudActor      `json:"actor"`
	Repository CloudRepository `json:"repository"`
	Issue      CloudIssue      `json:"issue"`
	Comment    CloudComment    `json:"comment"`
}

// CloudPullRequestEvent is a pullrequest:created / pullrequest:updated delivery,
// handed to Config.OnPullRequest.
type CloudPullRequestEvent struct {
	Actor       CloudActor       `json:"actor"`
	Repository  CloudRepository  `json:"repository"`
	PullRequest CloudPullRequest `json:"pullrequest"`
}

// CloudPushEvent is a repo:push delivery, handed to Config.OnPush. Push carries
// the change set; the callback filters refs itself, so only the actor and repo
// are decoded here and the rest rides along as RawMessage.
type CloudPushEvent struct {
	Actor      CloudActor      `json:"actor"`
	Repository CloudRepository `json:"repository"`
	Push       json.RawMessage `json:"push"`
}

// --- Data Center (REST 1.0 webhook) payloads ------------------------------

// ServerActor is the Data Center webhook actor. Identity keys on Slug (a stable
// username); Data Center webhooks carry no bot flag, so the reply-loop guard
// compares Slug against Config.Self.
type ServerActor struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	DisplayName string `json:"displayName"`
}

// ServerProject is a Data Center project; Key is the PROJECTKEY in a channel ID.
type ServerProject struct {
	Key string `json:"key"`
}

// ServerRepository is a Data Center repository.
type ServerRepository struct {
	Slug    string        `json:"slug"`
	Name    string        `json:"name"`
	Project ServerProject `json:"project"`
}

// ServerRef is a Data Center pull-request ref (source/target branch + repo).
type ServerRef struct {
	Repository ServerRepository `json:"repository"`
}

// ServerPullRequest is the minimal Data Center pull request. The repository lives
// under ToRef (the target branch's repo).
type ServerPullRequest struct {
	ID    int64     `json:"id"`
	Title string    `json:"title"`
	ToRef ServerRef `json:"toRef"`
}

// ServerComment is a Data Center pull-request comment; the body key is Text, not
// content.raw.
type ServerComment struct {
	ID          int64  `json:"id"`
	Text        string `json:"text"`
	CreatedDate int64  `json:"createdDate"` // epoch milliseconds
}

// ServerPullRequestCommentEvent is a pr:comment:added delivery.
type ServerPullRequestCommentEvent struct {
	Actor       ServerActor       `json:"actor"`
	PullRequest ServerPullRequest `json:"pullRequest"`
	Comment     ServerComment     `json:"comment"`
}

// ServerPullRequestEvent is a pr:opened / pr:from_ref_updated delivery, handed
// to Config.OnPullRequest.
type ServerPullRequestEvent struct {
	Actor       ServerActor       `json:"actor"`
	PullRequest ServerPullRequest `json:"pullRequest"`
}

// ServerPushEvent is a repo:refs_changed delivery, handed to Config.OnPush.
type ServerPushEvent struct {
	Actor      ServerActor      `json:"actor"`
	Repository ServerRepository `json:"repository"`
	Changes    json.RawMessage  `json:"changes"`
}

// --- Callback union types --------------------------------------------------

// PullRequestEvent is the typed pull-request payload handed to Config.OnPullRequest.
// Exactly one field is set, matching the bot's flavor.
type PullRequestEvent struct {
	Cloud  *CloudPullRequestEvent
	Server *ServerPullRequestEvent
}

// PushEvent is the typed push payload handed to Config.OnPush. Exactly one field
// is set, matching the bot's flavor.
type PushEvent struct {
	Cloud  *CloudPushEvent
	Server *ServerPushEvent
}

// --- Mapping to core.Message ----------------------------------------------

// newMessage folds the fields common to every comment kind into a core.Message.
// UserID is the actor identity the reply-loop guard compares against the bot's
// own resolved self (a Cloud account UUID, a Data Center user slug), so the drop
// check in server.go stays flavor-independent.
func newMessage(id int64, userID, authorName, channelID, content string, ts time.Time, raw *Message) *core.Message {
	return &core.Message{
		ID:         strconv.FormatInt(id, 10),
		UserID:     userID,
		AuthorName: authorName,
		ChannelID:  channelID,
		Content:    content,
		Timestamp:  ts,
		Raw:        raw,
	}
}

// parseCloudPRComment maps a Cloud pull-request comment to a core.Message.
// ChannelID is "workspace/repo!id" — Send's parseChannelID routes it to the PR
// comment endpoint.
func parseCloudPRComment(ev *CloudPullRequestCommentEvent) *core.Message {
	return newMessage(
		ev.Comment.ID,
		ev.Actor.UUID, ev.Actor.DisplayName,
		ev.Repository.FullName+"!"+strconv.FormatInt(ev.PullRequest.ID, 10),
		ev.Comment.Content.Raw,
		parseRFC3339(ev.Comment.CreatedOn),
		&Message{CloudPRComment: ev},
	)
}

// parseCloudIssueComment maps a Cloud issue comment to a core.Message. ChannelID
// is "workspace/repo#id" (issue notation, Cloud-only).
func parseCloudIssueComment(ev *CloudIssueCommentEvent) *core.Message {
	return newMessage(
		ev.Comment.ID,
		ev.Actor.UUID, ev.Actor.DisplayName,
		ev.Repository.FullName+"#"+strconv.FormatInt(ev.Issue.ID, 10),
		ev.Comment.Content.Raw,
		parseRFC3339(ev.Comment.CreatedOn),
		&Message{CloudIssueComment: ev},
	)
}

// parseServerPRComment maps a Data Center pull-request comment to a core.Message.
// ChannelID is "PROJECTKEY/repo!id", and the body lives under Comment.Text.
func parseServerPRComment(ev *ServerPullRequestCommentEvent) *core.Message {
	repo := ev.PullRequest.ToRef.Repository
	return newMessage(
		ev.Comment.ID,
		ev.Actor.Slug, ev.Actor.DisplayName,
		repo.Project.Key+"/"+repo.Slug+"!"+strconv.FormatInt(ev.PullRequest.ID, 10),
		ev.Comment.Text,
		millisToTime(ev.Comment.CreatedDate),
		&Message{ServerPRComment: ev},
	)
}

// parseRFC3339 best-effort-parses a Cloud webhook timestamp (ISO-8601). Timestamp
// is best-effort per core.Message's contract, so an unrecognized value yields the
// zero time rather than an error.
func parseRFC3339(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// millisToTime converts a Data Center epoch-millisecond timestamp to a time.Time,
// yielding the zero time for a zero/absent value (best-effort per core.Message).
func millisToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// --- Cloud flavor event routing + parsing ----------------------------------

func (*cloudFlavor) category(eventKey string) eventCategory {
	switch eventKey {
	case "pullrequest:comment_created", "issue:comment_created":
		return catComment
	case "pullrequest:created", "pullrequest:updated":
		return catPullRequest
	case "repo:push":
		return catPush
	default:
		return catUnknown
	}
}

func (*cloudFlavor) parseComment(eventKey string, payload []byte) (*core.Message, bool) {
	switch eventKey {
	case "issue:comment_created":
		var ev CloudIssueCommentEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			return nil, false
		}
		return parseCloudIssueComment(&ev), true
	default: // pullrequest:comment_created
		var ev CloudPullRequestCommentEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			return nil, false
		}
		return parseCloudPRComment(&ev), true
	}
}

func (*cloudFlavor) parsePullRequest(_ string, payload []byte) (*PullRequestEvent, string, bool) {
	var ev CloudPullRequestEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, "", false
	}
	return &PullRequestEvent{Cloud: &ev}, ev.Actor.UUID, true
}

func (*cloudFlavor) parsePush(_ string, payload []byte) (*PushEvent, bool) {
	var ev CloudPushEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, false
	}
	return &PushEvent{Cloud: &ev}, true
}

// --- Data Center flavor event routing + parsing ----------------------------

func (*serverFlavor) category(eventKey string) eventCategory {
	switch eventKey {
	case "pr:comment:added":
		return catComment
	case "pr:opened", "pr:from_ref_updated":
		return catPullRequest
	case "repo:refs_changed":
		return catPush
	default:
		return catUnknown
	}
}

func (*serverFlavor) parseComment(_ string, payload []byte) (*core.Message, bool) {
	var ev ServerPullRequestCommentEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, false
	}
	return parseServerPRComment(&ev), true
}

func (*serverFlavor) parsePullRequest(_ string, payload []byte) (*PullRequestEvent, string, bool) {
	var ev ServerPullRequestEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, "", false
	}
	return &PullRequestEvent{Server: &ev}, ev.Actor.Slug, true
}

func (*serverFlavor) parsePush(_ string, payload []byte) (*PushEvent, bool) {
	var ev ServerPushEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, false
	}
	return &PushEvent{Server: &ev}, true
}
