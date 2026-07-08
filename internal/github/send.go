package github

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	gogithub "github.com/google/go-github/v88/github"
)

// ErrBadChannelID is returned by Send when channelID is not "owner/repo#number".
var ErrBadChannelID = errors.New(`github: channel ID must be "owner/repo#number"`)

// parseChannelID splits "owner/repo#number" on the first '#' — owners and
// repos cannot contain '#', so a second one is malformed and fails the number
// parse rather than being silently absorbed into repo. The number must be a
// positive integer and both owner and repo must be non-empty.
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
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", 0, fmt.Errorf("%w: %q", ErrBadChannelID, channelID)
	}
	return owner, repo, number, nil
}

// Send posts text as a comment on the issue or PR identified by channelID
// ("owner/repo#number" — PRs are issues for commenting purposes). API errors
// are %w-wrapped, so callers can unwrap go-github's typed errors (e.g.
// *github.RateLimitError) with errors.As.
func (a *adapter) Send(ctx context.Context, channelID, text string) error {
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
