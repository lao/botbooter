// Package gitlab exposes the GitLab constructor, the raw-event accessor, and the
// Config/Message types for botbooter. Import it for a GitLab issue/merge-request
// ops bot: the adapter receives issue and merge-request comments over a Note Hook
// webhook and replies as notes through the GitLab REST API. Optional Config
// callbacks (OnMergeRequest, OnPush) route those deliveries on the same
// endpoint. A GitLab-only binary pulls in gitlab.com/gitlab-org/api/client-go but
// never compiles discordgo, slack-go, go-telegram or go-github.
package gitlab

import (
	gogitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/lao/botbooter"
	gitlabint "github.com/lao/botbooter/internal/gitlab"
)

// Config configures a GitLab bot. Token, Secret and Addr are required.
type Config = gitlabint.Config

// Message is the typed raw payload of an inbound Note Hook event: exactly one of
// IssueComment or MergeComment is set. Recover it with [RawEvent].
type Message = gitlabint.Message

// ErrMissingConfig is returned by [New] when a required [Config] field is empty.
var ErrMissingConfig = gitlabint.ErrMissingConfig

// ErrBadChannelID is returned by a GitLab bot's Send when the channel ID is not
// "group/project#iid" (issue) or "group/project!iid" (merge request). Branch it
// with errors.Is.
var ErrBadChannelID = gitlabint.ErrBadChannelID

// New creates a GitLab bot. It runs an inbound webhook HTTP server at cfg.Addr,
// so put a TLS-terminating proxy in front and register the public HTTPS URL as
// the project or group webhook (Comments trigger, plus Merge request/Push events
// when the matching callback is set, with cfg.Secret). It returns
// [ErrMissingConfig] on invalid config.
func New(cfg Config) (*botbooter.Bot, error) {
	return gitlabint.New(cfg)
}

// RawEvent returns the typed Note Hook payload carried on m, reporting whether m
// originated from GitLab. On a GitLab message exactly one of the returned
// [Message]'s IssueComment and MergeComment fields is non-nil.
func RawEvent(m *botbooter.Message) (*Message, bool) {
	return gitlabint.RawEvent(m)
}

// Client returns the underlying client-go client, or nil if b is not a GitLab
// bot. Use it for API calls beyond the adapter's send path (labels, awards,
// pipelines).
func Client(b *botbooter.Bot) *gogitlab.Client {
	return gitlabint.Client(b)
}

// Addr returns the address b's webhook listener is bound to (host:port), or ""
// if b is not a GitLab bot or is not connected. Use it to recover the
// OS-assigned port after passing cfg.Addr ":0".
func Addr(b *botbooter.Bot) string {
	return gitlabint.Addr(b)
}
