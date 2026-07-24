package gitlab

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	gogitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/lao/botbooter/internal/core"
)

// ErrBadChannelID is returned by Send when channelID is not
// "group/project#iid" (issue) or "group/project!iid" (merge request).
var ErrBadChannelID = errors.New(`gitlab: channel ID must be "group/project#iid" (issue) or "group/project!iid" (merge request)`)

// noteableKind is the kind of thing a note is attached to, recovered from the
// channel-id separator.
type noteableKind int

const (
	noteableIssue noteableKind = iota
	noteableMergeRequest
)

// parseChannelID splits "group/project#iid" (issue) or "group/project!iid"
// (merge request) on the first '#' or '!' — GitLab's native issue/MR notation.
// A project path (path_with_namespace) contains neither '#' nor '!', so the
// first one is unambiguously the separator; anything after a second one lands in
// the iid part and fails the number parse. The iid must be a positive integer,
// and the path must be at least namespace/project with every segment a valid
// GitLab path segment (see validSegment), so a crafted id cannot walk or inject
// the REST path the project is spliced into.
func parseChannelID(channelID string) (project string, iid int64, kind noteableKind, err error) {
	sep := strings.IndexAny(channelID, "#!")
	if sep < 0 {
		return "", 0, 0, fmt.Errorf("%w: %q", ErrBadChannelID, channelID)
	}
	switch channelID[sep] {
	case '#':
		kind = noteableIssue
	case '!':
		kind = noteableMergeRequest
	}
	iid, convErr := strconv.ParseInt(channelID[sep+1:], 10, 64)
	if convErr != nil || iid <= 0 {
		return "", 0, 0, fmt.Errorf("%w: %q", ErrBadChannelID, channelID)
	}
	project = channelID[:sep]
	segments := strings.Split(project, "/")
	if len(segments) < 2 {
		return "", 0, 0, fmt.Errorf("%w: %q", ErrBadChannelID, channelID)
	}
	for _, s := range segments {
		if !validSegment(s) {
			return "", 0, 0, fmt.Errorf("%w: %q", ErrBadChannelID, channelID)
		}
	}
	return project, iid, kind, nil
}

// validSegment reports whether s is a syntactically valid GitLab path segment:
// non-empty, neither "." nor ".." (a path-traversal segment), and drawn only
// from the [A-Za-z0-9._-] set, so nothing in it can introduce a path separator,
// query, or fragment when the project path is spliced into the REST URL. GitLab
// itself is stricter still, but rejecting everything outside this set is enough
// to keep a caller-supplied channel ID from escaping its path position.
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

// Send posts text as a note on the issue or merge request identified by
// channelID. SendOptions is ignored — issue and merge-request note threads are
// flat for the reply path (a note already lands in the originating
// conversation), so v1 does not implement threaded replies. API errors are
// %w-wrapped, so callers can unwrap client-go's typed errors with errors.As.
func (a *adapter) Send(ctx context.Context, channelID, text string, _ core.SendOptions) error {
	project, iid, kind, err := parseChannelID(channelID)
	if err != nil {
		return err
	}
	switch kind {
	case noteableIssue:
		_, _, err = a.client.Notes.CreateIssueNote(project, iid,
			&gogitlab.CreateIssueNoteOptions{Body: gogitlab.Ptr(text)}, gogitlab.WithContext(ctx))
	case noteableMergeRequest:
		_, _, err = a.client.Notes.CreateMergeRequestNote(project, iid,
			&gogitlab.CreateMergeRequestNoteOptions{Body: gogitlab.Ptr(text)}, gogitlab.WithContext(ctx))
	}
	if err != nil {
		return fmt.Errorf("gitlab: create note on %s: %w", channelID, err)
	}
	return nil
}
