package bitbucket

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	gobitbucket "github.com/ktrysmt/go-bitbucket"

	"github.com/lao/botbooter/internal/core"
)

// ErrBadChannelID is returned by Send when channelID is not "workspace/repo!id"
// (pull request) or "workspace/repo#id" (issue, Cloud only). On a Data Center bot
// the "#" issue form is rejected with the same sentinel wrapped "issue comments
// require Bitbucket Cloud", since Data Center has no issue tracker.
var ErrBadChannelID = errors.New(`bitbucket: channel ID must be "workspace/repo!id" (pull request) or "workspace/repo#id" (issue)`)

// cloudFlavor replies through the ktrysmt Cloud REST 2.0 client.
type cloudFlavor struct {
	client *gobitbucket.Client
}

// serverFlavor replies through plain net/http against a Data Center REST 1.0
// instance; baseURL already carries the /rest/api/1.0 prefix, and applyAuth
// stamps the per-mode credential onto each request.
type serverFlavor struct {
	baseURL   string
	http      *http.Client
	applyAuth func(*http.Request)
}

// commentTarget is a parsed channel ID: the workspace (Cloud) or project key
// (Data Center), the repo slug, the pull-request or issue number, and whether it
// names an issue.
type commentTarget struct {
	project string
	repo    string
	id      int64
	issue   bool
}

// parseChannelID splits "workspace/repo!id" (pull request) or "workspace/repo#id"
// (issue) on the first '!' or '#'. A workspace/project key and repo slug contain
// neither, so the first sigil is unambiguously the separator; anything after a
// second one lands in the id part and fails the number parse. The id must be a
// positive integer, and the path must be exactly two segments each drawn from the
// [A-Za-z0-9._~-] set (see validSegment), so a crafted channel ID cannot walk or
// inject the REST path it is spliced into.
func parseChannelID(channelID string) (commentTarget, error) {
	bad := func() (commentTarget, error) {
		return commentTarget{}, fmt.Errorf("%w: %q", ErrBadChannelID, channelID)
	}
	sep := strings.IndexAny(channelID, "#!")
	if sep < 0 {
		return bad()
	}
	id, err := strconv.ParseInt(channelID[sep+1:], 10, 64)
	if err != nil || id <= 0 {
		return bad()
	}
	segments := strings.Split(channelID[:sep], "/")
	if len(segments) != 2 || !validSegment(segments[0]) || !validSegment(segments[1]) {
		return bad()
	}
	return commentTarget{project: segments[0], repo: segments[1], id: id, issue: channelID[sep] == '#'}, nil
}

// validSegment reports whether s is a syntactically valid path segment: non-empty,
// neither "." nor ".." (a path-traversal segment), and drawn only from the
// [A-Za-z0-9._~-] set, so nothing in it can introduce a path separator, query, or
// fragment when spliced into the REST URL. '~' is admitted for Data Center
// personal-repository project keys, which are "~USERNAME" (payload
// project.key, type PERSONAL) and appear verbatim in the reply path
// /rest/api/1.0/projects/~USERNAME/repos/...; '~' is not a separator and cannot
// walk the path, so the traversal guarantee holds.
func validSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-', r == '~':
		default:
			return false
		}
	}
	return true
}

// parentID resolves the threading anchor from SendOptions: WithThreadID wins over
// InReplyTo, and either supplies a comment id the reply nests under. A zero value
// (or a non-positive / non-numeric id) means an unthreaded top-level comment.
func parentID(opts core.SendOptions) int64 {
	raw := opts.ThreadID
	if raw == "" && opts.ReplyTo != nil {
		raw = opts.ReplyTo.ID
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// Send posts text as a comment on the pull request or issue named by channelID.
// A zero SendOptions posts a top-level comment; InReplyTo/WithThreadID thread it
// under a parent comment (both flavors support parent.id natively). On a Data
// Center bot an issue channel ID is rejected — Data Center has no issue tracker.
// API errors are %w-wrapped.
func (a *adapter) Send(ctx context.Context, channelID, text string, opts core.SendOptions) error {
	target, err := parseChannelID(channelID)
	if err != nil {
		return err
	}
	if target.issue && a.dataCenter {
		return fmt.Errorf("%w: issue comments require Bitbucket Cloud", ErrBadChannelID)
	}
	return a.fl.postComment(ctx, target, text, parentID(opts))
}

// SendThreaded implements core.ThreadedSender: it nests the reply under the
// comment identified by replyToID. Bitbucket threads via a comment's parent id on
// both flavors, so this routes through Send with the id as the thread anchor.
func (a *adapter) SendThreaded(ctx context.Context, channelID, replyToID, text string) error {
	return a.Send(ctx, channelID, text, core.SendOptions{ThreadID: replyToID})
}

// postComment (Cloud) creates a pull-request or issue comment via ktrysmt. Issue
// comments are flat on Bitbucket Cloud, so parentID applies only to pull-request
// comments.
func (f *cloudFlavor) postComment(ctx context.Context, t commentTarget, text string, parent int64) error {
	if t.issue {
		opt := &gobitbucket.IssueCommentsOptions{
			IssuesOptions:  gobitbucket.IssuesOptions{Owner: t.project, RepoSlug: t.repo, ID: strconv.FormatInt(t.id, 10)},
			CommentContent: text,
		}
		opt.WithContext(ctx)
		if _, err := f.client.Repositories.Issues.CreateComment(opt); err != nil {
			return fmt.Errorf("bitbucket: create issue comment on %s/%s#%d: %w", t.project, t.repo, t.id, err)
		}
		return nil
	}
	opt := &gobitbucket.PullRequestCommentOptions{
		Owner:         t.project,
		RepoSlug:      t.repo,
		PullRequestID: strconv.FormatInt(t.id, 10),
		Content:       text,
	}
	if parent > 0 {
		p := int(parent)
		opt.Parent = &p
	}
	if _, err := f.client.Repositories.PullRequests.AddComment(opt.WithContext(ctx)); err != nil {
		return fmt.Errorf("bitbucket: create pull request comment on %s/%s!%d: %w", t.project, t.repo, t.id, err)
	}
	return nil
}

// postComment (Data Center) creates a pull-request comment over plain net/http.
// The reply body key is "text" (not Cloud's content.raw), and parent.id threads
// it. Send has already rejected an issue target on Data Center.
func (f *serverFlavor) postComment(ctx context.Context, t commentTarget, text string, parent int64) error {
	body := map[string]any{"text": text}
	if parent > 0 {
		body["parent"] = map[string]any{"id": parent}
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("bitbucket: marshal comment: %w", err)
	}
	url := fmt.Sprintf("%s/projects/%s/repos/%s/pull-requests/%d/comments", f.baseURL, t.project, t.repo, t.id)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("bitbucket: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	f.applyAuth(req)
	resp, err := f.http.Do(req)
	if err != nil {
		return fmt.Errorf("bitbucket: create pull request comment on %s/%s!%d: %w", t.project, t.repo, t.id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("bitbucket: create pull request comment on %s/%s!%d: unexpected status %d: %s",
			t.project, t.repo, t.id, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	// Drain a bounded amount of the success body so the keep-alive connection can
	// be reused rather than dropped when we close it.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return nil
}

// resolveSelf (Cloud) probes GET /2.0/user for the bot's account UUID. ktrysmt's
// Profile() takes no context, so the probe runs on a goroutine bounded by ctx —
// the goroutine still ends on the client's own HTTP timeout if the call hangs.
func (f *cloudFlavor) resolveSelf(ctx context.Context) (string, error) {
	type result struct {
		uuid string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		u, err := f.client.User.Profile()
		if err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{uuid: u.Uuid}
	}()
	select {
	case <-ctx.Done():
		return "", fmt.Errorf("bitbucket: resolve self identity: %w", ctx.Err())
	case r := <-ch:
		if r.err != nil {
			return "", fmt.Errorf("bitbucket: resolve self identity: %w", r.err)
		}
		return r.uuid, nil
	}
}

// resolveSelf (Data Center) has no whoami endpoint; Config.Self is required and
// validated at New, so this is never reached and returns empty.
func (f *serverFlavor) resolveSelf(_ context.Context) (string, error) {
	return "", nil
}
