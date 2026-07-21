package github

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	gogithub "github.com/google/go-github/v88/github"
	"github.com/lao/botbooter/internal/core"
)

const (
	defaultReactionPollInterval = 30 * time.Second

	// commentsPerPoll bounds each cycle's coverage window: only a repo's
	// this-many newest comments are checked, so a poll cycle costs at most
	// 1 + commentsPerPoll requests and reactions on older comments go
	// unseen. That partial coverage is the documented contract — full
	// coverage is unaffordable against the REST API.
	commentsPerPoll = 10
	// reactionsPerComment caps the reaction listing to one page; a comment
	// with more reactions than this loses the tail.
	reactionsPerComment = 100
)

// reactionStore records which reaction IDs the bot has already handled, so a
// poll cycle dispatches each reaction at most once. It is in-process only (a
// restart forgets what was handled; the connect-time cutoff is what prevents
// replay). Its seen set grows one int64 per dispatched reaction and is never
// pruned — negligible at poller rates, and the cutoff keeps pre-connect
// history out entirely. Reaction IDs are globally unique across GitHub, so
// one store serves all polled repositories.
type reactionStore struct {
	mu   sync.Mutex
	seen map[int64]struct{}
}

func newReactionStore() *reactionStore {
	return &reactionStore{seen: make(map[int64]struct{})}
}

// markSeen is an atomic check-and-set: it records id and reports whether it
// was fresh (unseen until now).
func (s *reactionStore) markSeen(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[id]; ok {
		return false
	}
	s.seen[id] = struct{}{}
	return true
}

// reactionEmoji maps the REST API's reaction content names to the unicode
// emoji they render as. Unicode is the form that satisfies core.Reaction's
// "renders as-is when sent back on its origin platform" contract: GitHub
// markdown renders every unicode emoji, while colon shortcodes exist for only
// some content names (":laugh:" is not a GitHub shortcode). The bare content
// name stays reachable via RawReaction.
var reactionEmoji = map[string]string{
	"+1": "👍", "-1": "👎", "laugh": "😄", "confused": "😕",
	"heart": "❤️", "hooray": "🎉", "rocket": "🚀", "eyes": "👀",
}

// ReactionPayload is the typed raw payload stored in core.Reaction.Raw for
// GitHub bots: the reaction and the issue comment it was added to. There is no
// webhook event to carry — GitHub sends none for reactions; the adapter
// discovered the reaction by polling (see Config.ReactionPollRepos).
type ReactionPayload struct {
	Reaction *gogithub.Reaction
	Comment  *gogithub.IssueComment
}

// RawReaction returns the typed reaction payload carried on r, reporting
// whether r originated from GitHub. Reaction.GetContent gives the bare REST
// content name ("+1", "hooray") behind the unicode Emoji.
func RawReaction(r *core.Reaction) (*ReactionPayload, bool) {
	v, ok := r.Raw.(*ReactionPayload)
	return v, ok
}

// repoRef is one validated ReactionPollRepos entry.
type repoRef struct{ owner, name string }

func (r repoRef) String() string { return r.owner + "/" + r.name }

// parsePollRepos validates ReactionPollRepos entries into owner/name pairs.
// Duplicates collapse to one entry: the store would suppress double dispatch
// anyway, but each copy would still cost its own API requests every cycle.
func parsePollRepos(entries []string) ([]repoRef, error) {
	repos := make([]repoRef, 0, len(entries))
	seen := make(map[repoRef]struct{}, len(entries))
	for _, entry := range entries {
		owner, name, ok := strings.Cut(entry, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf(`%w: ReactionPollRepos entry %q must be "owner/name"`, ErrBadReactionConfig, entry)
		}
		ref := repoRef{owner: owner, name: name}
		if _, dup := seen[ref]; dup {
			continue
		}
		seen[ref] = struct{}{}
		repos = append(repos, ref)
	}
	return repos, nil
}

// issueNumber extracts the issue number from a comment's API issue URL
// (".../repos/owner/repo/issues/42"); the comment payload does not carry the
// number directly.
func issueNumber(comment *gogithub.IssueComment) (int, error) {
	url := comment.GetIssueURL()
	n, err := strconv.Atoi(url[strings.LastIndex(url, "/")+1:])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("github: no issue number in comment issue_url %q", url)
	}
	return n, nil
}

// toReaction maps one polled reaction into the platform-agnostic form.
// ChannelID is "owner/repo#number" (what Send accepts) and MessageID is the
// reacted comment's ID, so ReplyToMessage lands in the same issue thread.
func toReaction(repo repoRef, issue int, comment *gogithub.IssueComment, reaction *gogithub.Reaction) *core.Reaction {
	emoji, ok := reactionEmoji[reaction.GetContent()]
	if !ok {
		emoji = reaction.GetContent() // future content names degrade to the raw name
	}
	return &core.Reaction{
		Emoji:      emoji,
		UserID:     strconv.FormatInt(reaction.GetUser().GetID(), 10),
		AuthorName: reaction.GetUser().GetLogin(),
		ChannelID:  repo.String() + "#" + strconv.Itoa(issue),
		MessageID:  strconv.FormatInt(comment.GetID(), 10),
		Raw:        &ReactionPayload{Reaction: reaction, Comment: comment},
	}
}

// pollReactions is the reaction ingress loop, one goroutine per connection.
// GitHub sends no webhook for reactions, so it diffs each polled repo's newest
// comments every ReactionPollInterval and dispatches reactions that pass the
// cutoff, the self/bot filter and the store's freshness check. ctx is the
// connection's run context and is the only thing that stops the loop; poll
// failures are logged and retried next cycle, never fatal — the webhook half
// of the adapter keeps serving. Dispatch runs on dispatchCtx (the connection's
// detached context) under the inflight counter, so Disconnect's drain covers
// reaction handlers like webhook dispatch — minus the ack barrier: webhook
// increments land before Shutdown returns, while a poller mid-cycle can spawn
// a dispatch as the drain ends, so that handler runs with an already-canceled
// context and may outlive Disconnect.
func (a *adapter) pollReactions(ctx, dispatchCtx context.Context, deps core.AdapterDeps, cutoff time.Time) {
	// counts caches each window comment's reactions.total_count from the
	// previous cycle; an unchanged count skips the per-comment detail request,
	// holding the steady-state cost at one list request per repo per cycle.
	// It is rebuilt every cycle so comments that scroll out of the window do
	// not accumulate.
	counts := make(map[int64]int)
	ticker := time.NewTicker(a.cfg.ReactionPollInterval)
	defer ticker.Stop()
	for {
		next := make(map[int64]int, len(counts))
		for _, repo := range a.pollRepos {
			if err := a.pollRepoOnce(ctx, dispatchCtx, deps, repo, counts, next, cutoff); err != nil {
				if ctx.Err() != nil {
					return
				}
				a.log().Warn("github: reaction poll failed", "repo", repo.String(), "error", err)
			}
		}
		counts = next
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pollRepoOnce runs one repo's poll cycle: list the newest comments, then list
// reactions only for comments whose total count changed since the previous
// cycle. A comment's count moves from prev to next only after its reactions
// were handled, so an error retries that comment next cycle instead of
// silently dropping its reactions.
func (a *adapter) pollRepoOnce(ctx, dispatchCtx context.Context, deps core.AdapterDeps, repo repoRef, prev, next map[int64]int, cutoff time.Time) error {
	// Issue number 0 lists comments across every issue and PR in the repo,
	// which is what makes one request per repo sufficient.
	comments, _, err := a.client.Issues.ListComments(ctx, repo.owner, repo.name, 0, &gogithub.IssueListCommentsOptions{
		Sort:        gogithub.Ptr("created"),
		Direction:   gogithub.Ptr("desc"),
		ListOptions: gogithub.ListOptions{PerPage: commentsPerPoll},
	})
	if err != nil {
		return fmt.Errorf("list comments: %w", err)
	}
	for _, comment := range comments {
		count := comment.GetReactions().GetTotalCount()
		if cached, ok := prev[comment.GetID()]; (ok && cached == count) || count == 0 {
			next[comment.GetID()] = count
			continue
		}
		if err := a.dispatchCommentReactions(ctx, dispatchCtx, deps, repo, comment, cutoff); err != nil {
			return err
		}
		next[comment.GetID()] = count
	}
	return nil
}

// dispatchCommentReactions lists one comment's reactions and dispatches the
// ones not yet handled.
func (a *adapter) dispatchCommentReactions(ctx, dispatchCtx context.Context, deps core.AdapterDeps, repo repoRef, comment *gogithub.IssueComment, cutoff time.Time) error {
	issue, err := issueNumber(comment)
	if err != nil {
		return err
	}
	reactions, _, err := a.client.Reactions.ListIssueCommentReactions(ctx, repo.owner, repo.name, comment.GetID(),
		&gogithub.ListReactionOptions{ListOptions: gogithub.ListOptions{PerPage: reactionsPerComment}})
	if err != nil {
		return fmt.Errorf("list reactions for comment %d: %w", comment.GetID(), err)
	}
	for _, reaction := range reactions {
		// Cutoff first, store second: reactions from before the window are
		// never inserted into the store, which keeps its seen set from
		// accumulating history the cutoff already excludes.
		if !reaction.GetCreatedAt().After(cutoff) {
			continue
		}
		if a.isSelfOrBotUser(reaction.GetUser()) {
			continue
		}
		if !a.reactions.markSeen(reaction.GetID()) {
			continue
		}
		r := toReaction(repo, issue, comment, reaction)
		a.inflight.Add(1)
		go func() {
			defer a.inflight.Add(-1)
			deps.DispatchReaction(dispatchCtx, r)
		}()
	}
	return nil
}
