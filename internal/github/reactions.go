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

	// reactionPollBudgetPerHour is the poller's share of the API rate limit:
	// 3000 of a PAT's 5000 core requests per hour, leaving headroom for
	// discovery pages, changed-count detail requests (worst case ~11 per repo
	// per cycle), the bot's own replies and the consumer's other API use via
	// Client. Steady-state poll cost is one request per repo per cycle, so the
	// budget caps repos/interval — see effectiveInterval.
	reactionPollBudgetPerHour = 3000

	// discoveryRefreshCycles is how many poll cycles a wildcard owner's
	// discovered repo list is reused before it is re-listed, so new
	// repositories are picked up without paying discovery requests every
	// cycle.
	discoveryRefreshCycles = 10

	// reposPerDiscoveryPage is the page size for wildcard repo discovery; 100
	// is the API maximum, so discovery costs ~1 request per 100 repos.
	reposPerDiscoveryPage = 100
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

// repoRefOf maps an API repository to its poll reference.
func repoRefOf(repo *gogithub.Repository) repoRef {
	return repoRef{owner: repo.GetOwner().GetLogin(), name: repo.GetName()}
}

// parsePollRepos validates ReactionPollRepos entries into explicit owner/name
// pairs and wildcard owners ("owner/*" — poll every repo of that owner).
// Duplicates of either kind collapse to one entry: the store would suppress
// double dispatch anyway, but each copy would still cost its own API requests
// every cycle. "*" is only special as the whole name; a name merely containing
// it stays a literal repo name for the API to reject.
func parsePollRepos(entries []string) (repos []repoRef, wildcardOwners []string, err error) {
	seen := make(map[repoRef]struct{}, len(entries))
	seenOwners := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		owner, name, ok := strings.Cut(entry, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, nil, fmt.Errorf(`%w: ReactionPollRepos entry %q must be "owner/name" or "owner/*"`, ErrBadReactionConfig, entry)
		}
		if name == "*" {
			if _, dup := seenOwners[owner]; dup {
				continue
			}
			seenOwners[owner] = struct{}{}
			wildcardOwners = append(wildcardOwners, owner)
			continue
		}
		ref := repoRef{owner: owner, name: name}
		if _, dup := seen[ref]; dup {
			continue
		}
		seen[ref] = struct{}{}
		repos = append(repos, ref)
	}
	return repos, wildcardOwners, nil
}

// mergeRepos joins explicit and discovered repos, dropping duplicates so a
// repo both listed explicitly and matched by a wildcard is polled once.
func mergeRepos(explicit, discovered []repoRef) []repoRef {
	out := make([]repoRef, 0, len(explicit)+len(discovered))
	seen := make(map[repoRef]struct{}, len(explicit)+len(discovered))
	for _, refs := range [][]repoRef{explicit, discovered} {
		for _, ref := range refs {
			if _, dup := seen[ref]; dup {
				continue
			}
			seen[ref] = struct{}{}
			out = append(out, ref)
		}
	}
	return out
}

// effectiveInterval sizes the poll interval against the API request budget.
// Steady-state cost is one request per repo per cycle, so the smallest
// budget-fitting interval is repos*3600/budget seconds (rounded up). It
// returns the interval to poll at and whether the configured interval would
// exceed the budget (the caller's cue to warn): under budget the configured
// interval stands; over budget it is raised to fit, unless auto-raising is
// disabled (Config.ReactionPollNoAutoInterval), in which case the configured
// interval stands anyway and only the warning remains.
func effectiveInterval(repos int, configured time.Duration, noAuto bool) (time.Duration, bool) {
	seconds := (repos*3600 + reactionPollBudgetPerHour - 1) / reactionPollBudgetPerHour
	minInterval := time.Duration(seconds) * time.Second
	if minInterval <= configured {
		return configured, false
	}
	if noAuto {
		return configured, true
	}
	return minInterval, true
}

// budgetState is the (repo count, effective interval) pair the poller last
// warned about, so the budget warning fires when the situation changes, not
// every cycle.
type budgetState struct {
	repos    int
	interval time.Duration
}

// pollInterval returns the interval for the coming cycle and emits the budget
// warning on transitions: entering the over-budget state, or staying in it
// with a different repo count or interval. Dropping back under budget resets
// warned so a later re-entry warns again.
func (a *adapter) pollInterval(repos int, warned *budgetState) time.Duration {
	interval, exceeded := effectiveInterval(repos, a.cfg.ReactionPollInterval, a.cfg.ReactionPollNoAutoInterval)
	if !exceeded {
		*warned = budgetState{}
		return interval
	}
	state := budgetState{repos: repos, interval: interval}
	if *warned != state {
		*warned = state
		if a.cfg.ReactionPollNoAutoInterval {
			a.log().Warn("github: reaction polling exceeds the API request budget and automatic interval adjustment is disabled",
				"repos", repos, "interval", interval, "budget_requests_per_hour", reactionPollBudgetPerHour)
		} else {
			a.log().Warn("github: reaction polling would exceed the API request budget; automatically raising the poll interval",
				"repos", repos, "configured_interval", a.cfg.ReactionPollInterval, "effective_interval", interval)
		}
	}
	return interval
}

// discoverRepos resolves the wildcard owners into concrete repositories.
// Archived repos are skipped — reactions are locked on them, so polling would
// be pure waste. In App mode the installation's repo set (which already
// scopes what the App can see) is listed once and filtered per owner; in PAT
// mode each owner is listed directly — the authenticated user's own repos
// (private included) when the owner is the bot itself, otherwise the owner's
// public repos.
func (a *adapter) discoverRepos(ctx context.Context) ([]repoRef, error) {
	if a.cfg.Token == "" {
		return a.discoverInstallationRepos(ctx)
	}
	var out []repoRef
	for _, owner := range a.wildcardOwners {
		repos, err := a.listOwnerRepos(ctx, owner)
		if err != nil {
			return nil, err
		}
		out = append(out, repos...)
	}
	return out, nil
}

func (a *adapter) discoverInstallationRepos(ctx context.Context) ([]repoRef, error) {
	var out []repoRef
	page := 1
	for {
		list, resp, err := a.client.Apps.ListRepos(ctx, &gogithub.ListOptions{PerPage: reposPerDiscoveryPage, Page: page})
		if err != nil {
			return nil, fmt.Errorf("list installation repos: %w", err)
		}
		for _, repo := range list.Repositories {
			if repo.GetArchived() || !a.isWildcardOwner(repo.GetOwner().GetLogin()) {
				continue
			}
			out = append(out, repoRefOf(repo))
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		page = resp.NextPage
	}
}

// isWildcardOwner reports whether login matches one of the configured
// "owner/*" wildcard owners (GitHub logins are case-insensitive).
func (a *adapter) isWildcardOwner(login string) bool {
	for _, owner := range a.wildcardOwners {
		if strings.EqualFold(login, owner) {
			return true
		}
	}
	return false
}

func (a *adapter) listOwnerRepos(ctx context.Context, owner string) ([]repoRef, error) {
	a.mu.Lock()
	self := a.selfLogin
	a.mu.Unlock()
	// The bot's own repos: /users/{owner}/repos would hide private ones, so
	// use the authenticated-user listing, owner-affiliated.
	selfOwned := self != "" && strings.EqualFold(owner, self)
	var out []repoRef
	page := 1
	for {
		var repos []*gogithub.Repository
		var resp *gogithub.Response
		var err error
		if selfOwned {
			repos, resp, err = a.client.Repositories.ListByAuthenticatedUser(ctx, &gogithub.RepositoryListByAuthenticatedUserOptions{
				Affiliation: "owner",
				ListOptions: gogithub.ListOptions{PerPage: reposPerDiscoveryPage, Page: page},
			})
		} else {
			repos, resp, err = a.client.Repositories.ListByUser(ctx, owner, &gogithub.RepositoryListByUserOptions{
				ListOptions: gogithub.ListOptions{PerPage: reposPerDiscoveryPage, Page: page},
			})
		}
		if err != nil {
			return nil, fmt.Errorf("list repos of %s: %w", owner, err)
		}
		for _, repo := range repos {
			if repo.GetArchived() {
				continue
			}
			out = append(out, repoRefOf(repo))
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		page = resp.NextPage
	}
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
// comments every cycle and dispatches reactions that pass the cutoff, the
// self/bot filter and the store's freshness check. The polled set is the
// explicit ReactionPollRepos entries plus the wildcard owners' discovered
// repos; the cycle interval is ReactionPollInterval, raised when the repo
// count would blow the API request budget (see pollInterval). ctx is the
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
	// discovered holds the wildcard owners' resolved repos, refreshed every
	// discoveryRefreshCycles cycles. A failed refresh keeps the previous set
	// (empty until the first refresh succeeds) — explicit repos are unaffected
	// and the next refresh retries.
	var discovered []repoRef
	var warned budgetState
	for cycle := 0; ; cycle++ {
		if len(a.wildcardOwners) > 0 && cycle%discoveryRefreshCycles == 0 {
			repos, err := a.discoverRepos(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				a.log().Warn("github: reaction repo discovery failed", "error", err)
			} else {
				discovered = repos
			}
		}
		repos := mergeRepos(a.pollRepos, discovered)
		// The interval is recomputed every cycle because discovery moves the
		// repo count; pollInterval warns when the budget forces (or, with
		// auto-adjust disabled, would force) a longer interval than configured.
		interval := a.pollInterval(len(repos), &warned)
		next := make(map[int64]int, len(counts))
		for _, repo := range repos {
			if err := a.pollRepoOnce(ctx, dispatchCtx, deps, repo, counts, next, cutoff); err != nil {
				if ctx.Err() != nil {
					return
				}
				a.log().Warn("github: reaction poll failed", "repo", repo.String(), "error", err)
			}
		}
		counts = next
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
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
