// Package github exposes the GitHub constructor, the raw-event accessor, and
// the Config/Message types for botbooter. Import it for a GitHub issue-ops bot:
// the adapter receives issue and PR comments over an issue_comment webhook and
// replies as issue comments through the GitHub REST API. A GitHub-only binary
// pulls in go-github and ghinstallation but never compiles discordgo, slack-go
// or go-telegram.
package github

import (
	gogithub "github.com/google/go-github/v88/github"
	"github.com/lao/botbooter"
	githubint "github.com/lao/botbooter/internal/github"
)

// Config configures a GitHub bot. Exactly one auth mode must be set: Token
// (PAT) or the AppID/InstallationID/PrivateKey triple (GitHub App).
type Config = githubint.Config

// Message is the typed raw payload of an inbound issue_comment event.
type Message = githubint.Message

// ErrMissingConfig is returned by [New] when a required [Config] field is empty.
var ErrMissingConfig = githubint.ErrMissingConfig

// ErrAmbiguousAuth is returned by [New] when both auth modes are configured.
var ErrAmbiguousAuth = githubint.ErrAmbiguousAuth

// ErrBadChannelID is returned by a GitHub bot's Send when the channel ID is not
// "owner/repo#number". Branch it with errors.Is.
var ErrBadChannelID = githubint.ErrBadChannelID

// New creates a GitHub bot. It runs an inbound webhook HTTP server at cfg.Addr,
// so put a TLS-terminating proxy in front and register the public HTTPS URL as
// the repository or App webhook (content type application/json, issue_comment
// events, with cfg.WebhookSecret). It returns [ErrMissingConfig] or
// [ErrAmbiguousAuth] on invalid config.
func New(cfg Config) (*botbooter.Bot, error) {
	return githubint.New(cfg)
}

// RawEvent returns the typed issue_comment event carried on m, reporting
// whether m originated from GitHub.
func RawEvent(m *botbooter.Message) (*Message, bool) {
	return githubint.RawEvent(m)
}

// Client returns the underlying go-github client, or nil if b is not a GitHub
// bot. Use it for API calls beyond the adapter's send path (labels, reactions,
// checks).
func Client(b *botbooter.Bot) *gogithub.Client {
	return githubint.Client(b)
}

// Addr returns the address b's webhook listener is bound to (host:port), or ""
// if b is not a GitHub bot or is not connected. Use it to recover the
// OS-assigned port after passing cfg.Addr ":0".
func Addr(b *botbooter.Bot) string {
	return githubint.Addr(b)
}
