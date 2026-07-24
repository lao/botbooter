package github

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	gogithub "github.com/google/go-github/v88/github"

	"github.com/lao/botbooter/internal/core"
)

// ErrBadChannelID is returned by Send when channelID is not "owner/repo#number".
var ErrBadChannelID = errors.New(`github: channel ID must be "owner/repo#number"`)

// parseChannelID splits "owner/repo#number" on the first '#' — owners and
// repos cannot contain '#', so a second one is malformed and fails the number
// parse rather than being silently absorbed into repo. The number must be a
// positive integer and both owner and repo must be valid GitHub name segments
// (see validSegment), so a crafted id cannot walk or inject the REST path the
// segments are spliced into.
func parseChannelID(channelID string) (owner, repo string, number int, err error) {
	hash := strings.Index(channelID, "#")
	if hash < 0 {
		return "", "", 0, fmt.Errorf("%w: %q", ErrBadChannelID, channelID)
	}
	number, convErr := strconv.Atoi(channelID[hash+1:])
	if convErr != nil || number <= 0 {
		return "", "", 0, fmt.Errorf("%w: %q", ErrBadChannelID, channelID)
	}
	owner, repo, ok := strings.Cut(channelID[:hash], "/")
	if !ok || !validSegment(owner) || !validSegment(repo) {
		return "", "", 0, fmt.Errorf("%w: %q", ErrBadChannelID, channelID)
	}
	return owner, repo, number, nil
}

// validSegment reports whether s is a syntactically valid owner or repo
// segment: non-empty, neither "." nor ".." (a path-traversal segment), and
// drawn only from the GitHub-legal [A-Za-z0-9._-] set, so nothing in it can
// introduce a path separator, query, or fragment when spliced into the REST
// URL. GitHub itself is stricter still, but rejecting everything outside this
// set is enough to keep a caller-supplied channel ID from escaping its path
// position.
func validSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// Send posts text as a comment on the issue or PR identified by channelID
// ("owner/repo#number" — PRs are issues for commenting purposes). SendOptions
// is ignored — issue comment threads are flat, so a reply already lands in the
// originating conversation. API errors are %w-wrapped, so callers can unwrap
// go-github's typed errors (e.g. *github.RateLimitError) with errors.As.
func (a *adapter) Send(ctx context.Context, channelID, text string, _ core.SendOptions) error {
	owner, repo, number, err := parseChannelID(channelID)
	if err != nil {
		return err
	}
	_, _, err = a.client.Issues.CreateComment(ctx, owner, repo, number,
		&gogithub.IssueComment{Body: gogithub.Ptr(text)})
	if err != nil {
		return fmt.Errorf("github: create comment on %s: %w", channelID, err)
	}
	return nil
}
