// Package bitbucket exposes the Bitbucket constructor, the raw-event accessor,
// and the Config/Message types for botbooter. Import it for a Bitbucket
// pull-request ops bot: the adapter receives PR and issue comments over a webhook
// and replies as comments through the Bitbucket REST API. One package serves both
// flavors, Cloud and Data Center, selected by Config.BaseURL. Optional Config
// callbacks (OnPullRequest, OnPush) route those deliveries on the same endpoint.
// A Bitbucket-only binary pulls in github.com/ktrysmt/go-bitbucket (its Cloud
// client) but never compiles another platform's SDK.
package bitbucket

import (
	gobitbucket "github.com/ktrysmt/go-bitbucket"

	"github.com/lao/botbooter"
	bitbucketint "github.com/lao/botbooter/internal/bitbucket"
)

// Config configures a Bitbucket bot. Secret and Addr are required, as is exactly
// one auth mode (Email+APIToken or AccessToken). Self is required in access-token
// mode and on Data Center.
type Config = bitbucketint.Config

// Message is the typed raw payload of an inbound comment event: exactly one of
// CloudPRComment, CloudIssueComment or ServerPRComment is set. Recover it with
// [RawEvent].
type Message = bitbucketint.Message

// The typed callback payloads for Config.OnPullRequest / Config.OnPush; exactly
// one field of each union is set, matching the bot's flavor.
type (
	// PullRequestEvent is handed to Config.OnPullRequest.
	PullRequestEvent = bitbucketint.PullRequestEvent
	// PushEvent is handed to Config.OnPush.
	PushEvent = bitbucketint.PushEvent
)

// The typed webhook event payloads carried on [Message] and the callback unions,
// re-exported so a consumer can name them when reading a raw event.
type (
	// CloudPullRequestCommentEvent is a Cloud pullrequest:comment_created payload.
	CloudPullRequestCommentEvent = bitbucketint.CloudPullRequestCommentEvent
	// CloudIssueCommentEvent is a Cloud issue:comment_created payload.
	CloudIssueCommentEvent = bitbucketint.CloudIssueCommentEvent
	// ServerPullRequestCommentEvent is a Data Center pr:comment:added payload.
	ServerPullRequestCommentEvent = bitbucketint.ServerPullRequestCommentEvent
	// CloudPullRequestEvent is a Cloud pullrequest:created/updated payload.
	CloudPullRequestEvent = bitbucketint.CloudPullRequestEvent
	// ServerPullRequestEvent is a Data Center pr:opened/pr:from_ref_updated payload.
	ServerPullRequestEvent = bitbucketint.ServerPullRequestEvent
	// CloudPushEvent is a Cloud repo:push payload.
	CloudPushEvent = bitbucketint.CloudPushEvent
	// ServerPushEvent is a Data Center repo:refs_changed payload.
	ServerPushEvent = bitbucketint.ServerPushEvent
)

// ErrMissingConfig is returned by [New] when a required [Config] field is empty.
var ErrMissingConfig = bitbucketint.ErrMissingConfig

// ErrAmbiguousAuth is returned by [New] when neither or both auth modes are set;
// exactly one of Email+APIToken or AccessToken is required.
var ErrAmbiguousAuth = bitbucketint.ErrAmbiguousAuth

// ErrBadChannelID is returned by a Bitbucket bot's Send when the channel ID is
// not "workspace/repo!id" (pull request) or "workspace/repo#id" (issue, Cloud
// only). Branch it with errors.Is.
var ErrBadChannelID = bitbucketint.ErrBadChannelID

// New creates a Bitbucket bot. It runs an inbound webhook HTTP server at
// cfg.Addr, so put a TLS-terminating proxy in front and register the public
// HTTPS URL as the repository webhook (comment trigger, plus PR/push triggers
// when the matching callback is set, with cfg.Secret). It returns
// [ErrMissingConfig] or [ErrAmbiguousAuth] on invalid config.
func New(cfg Config) (*botbooter.Bot, error) {
	return bitbucketint.New(cfg)
}

// RawEvent returns the typed webhook payload carried on m, reporting whether m
// originated from Bitbucket. On a Bitbucket message exactly one of the returned
// [Message]'s CloudPRComment, CloudIssueComment and ServerPRComment fields is
// non-nil.
func RawEvent(m *botbooter.Message) (*Message, bool) {
	return bitbucketint.RawEvent(m)
}

// CloudClient returns the underlying ktrysmt Cloud client, or nil if b is not a
// Bitbucket bot or is a Data Center bot (whose replies go out over plain
// net/http, with no Cloud client). Use it for Cloud REST 2.0 calls beyond the
// adapter's send path.
func CloudClient(b *botbooter.Bot) *gobitbucket.Client {
	return bitbucketint.CloudClient(b)
}

// Addr returns the address b's webhook listener is bound to (host:port), or "" if
// b is not a Bitbucket bot or is not connected. Use it to recover the OS-assigned
// port after passing cfg.Addr ":0".
func Addr(b *botbooter.Bot) string {
	return bitbucketint.Addr(b)
}
