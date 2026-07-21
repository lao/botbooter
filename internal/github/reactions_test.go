package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v88/github"
	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// fakeAPI is a hermetic stand-in for the GitHub REST endpoints the reaction
// poller hits: the repo-wide comment list, per-comment reactions, and /user
// for PAT self-resolution during Connect. Call counters let tests pin the
// poller's API cost.
type fakeAPI struct {
	mu            sync.Mutex
	comments      []*gogithub.IssueComment
	reactions     map[int64][]*gogithub.Reaction
	commentLists  int
	reactionLists int
	hits          int // every request, including failed ones
	fail          bool
}

func (f *fakeAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.hits++
		if f.fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"id": 777, "login": "botbooter-bot"}`))
		case r.URL.Path == "/repos/lao/botbooter/issues/comments":
			f.commentLists++
			writeJSON(t, w, f.comments)
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			f.reactionLists++
			parts := strings.Split(r.URL.Path, "/")
			id, err := strconv.ParseInt(parts[len(parts)-2], 10, 64)
			asserts.NoError(t, err, "comment id in reactions path")
			writeJSON(t, w, f.reactions[id])
		default:
			t.Errorf("unexpected API path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode fake API response: %v", err)
	}
}

func (f *fakeAPI) counts() (commentLists, reactionLists int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commentLists, f.reactionLists
}

func testComment(id int64, issue, totalReactions int) *gogithub.IssueComment {
	return &gogithub.IssueComment{
		ID:        gogithub.Ptr(id),
		IssueURL:  gogithub.Ptr(fmt.Sprintf("https://api.github.com/repos/lao/botbooter/issues/%d", issue)),
		Reactions: &gogithub.Reactions{TotalCount: gogithub.Ptr(totalReactions)},
	}
}

func testReaction(id int64, content string, userID int64, userType string, created time.Time) *gogithub.Reaction {
	return &gogithub.Reaction{
		ID:      gogithub.Ptr(id),
		Content: gogithub.Ptr(content),
		User: &gogithub.User{
			ID:    gogithub.Ptr(userID),
			Login: gogithub.Ptr("alice"),
			Type:  gogithub.Ptr(userType),
		},
		CreatedAt: &gogithub.Timestamp{Time: created},
	}
}

// storeSize reports how many reaction IDs the adapter's dedup store holds.
func storeSize(a *adapter) int {
	a.reactions.mu.Lock()
	defer a.reactions.mu.Unlock()
	return len(a.reactions.seen)
}

func pollConfig() Config {
	cfg := patConfig()
	cfg.ReactionPollRepos = []string{"lao/botbooter"}
	return cfg
}

func pollAdapter(t *testing.T, cfg Config, srv *httptest.Server) *adapter {
	t.Helper()
	a, err := newAdapter(cfg)
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)
	return a
}

// reactionCaptureDeps mirrors captureDeps for the reaction ingress path,
// modeling a bot with an OnReaction handler registered (HasReactionHandlers).
// The mutex matters: each reaction dispatches on its own goroutine.
func reactionCaptureDeps(mu *sync.Mutex, got *[]*core.Reaction, done chan struct{}) core.AdapterDeps {
	return core.AdapterDeps{
		Dispatch:            func(context.Context, *core.Message) {},
		HasReactionHandlers: true,
		DispatchReaction: func(_ context.Context, r *core.Reaction) {
			mu.Lock()
			*got = append(*got, r)
			mu.Unlock()
			if done != nil {
				done <- struct{}{}
			}
		},
		Done:       func(error) {},
		Disconnect: func() error { return nil },
	}
}

// drainInflight settles any dispatch goroutines a poll cycle spawned before a
// test asserts on them. The inflight increment is synchronous with the spawn,
// so a "nothing dispatched" assertion after this drain is deterministic: a
// wrongly-spawned dispatch is waited for and lands in got, instead of racing
// the assertion and false-passing.
func drainInflight(t *testing.T, a *adapter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a.drainDispatch(ctx)
	// drainDispatch returns silently on timeout; a hung dispatch must fail the
	// test, not let a "nothing dispatched" assertion false-pass.
	asserts.Equal(t, a.inflight.Load(), int64(0), "dispatch goroutines settled within the drain window")
}

var testRepo = repoRef{owner: "lao", name: "botbooter"}

func TestNewAdapter_ReactionConfigValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"NoSlash", func(c *Config) { c.ReactionPollRepos = []string{"laobotbooter"} }},
		{"EmptyOwner", func(c *Config) { c.ReactionPollRepos = []string{"/botbooter"} }},
		{"EmptyName", func(c *Config) { c.ReactionPollRepos = []string{"lao/"} }},
		{"ExtraSlash", func(c *Config) { c.ReactionPollRepos = []string{"lao/botbooter/extra"} }},
		{"NegativeInterval", func(c *Config) { c.ReactionPollInterval = -time.Second }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := pollConfig()
			tc.mutate(&cfg)
			_, err := newAdapter(cfg)
			asserts.ErrorIs(t, err, ErrBadReactionConfig, tc.name)
		})
	}
}

func TestNewAdapter_ReactionDefaults(t *testing.T) {
	a, err := newAdapter(pollConfig())
	asserts.NoError(t, err, "valid poll config")
	asserts.Equal(t, a.cfg.ReactionPollInterval, defaultReactionPollInterval, "default interval")
	asserts.True(t, a.reactions != nil, "in-memory store allocated when polling is on")
	asserts.Equal(t, len(a.pollRepos), 1, "one parsed repo")
	asserts.Equal(t, a.pollRepos[0], testRepo, "parsed owner/name")
}

// Duplicate ReactionPollRepos entries must collapse to one: the store would
// suppress double dispatch anyway, but each copy would still cost its own API
// requests every cycle.
func TestNewAdapter_DuplicatePollReposCollapse(t *testing.T) {
	cfg := pollConfig()
	cfg.ReactionPollRepos = []string{"lao/botbooter", "lao/botbooter", "lao/other"}
	a, err := newAdapter(cfg)
	asserts.NoError(t, err, "duplicates are not a config error")
	asserts.Equal(t, len(a.pollRepos), 2, "duplicate collapsed")
	asserts.Equal(t, a.pollRepos[0], testRepo, "first entry kept")
	asserts.Equal(t, a.pollRepos[1], repoRef{owner: "lao", name: "other"}, "distinct entry kept")
}

// Zero-config bots must not pay for the feature: no parsed repos, no store.
func TestNewAdapter_NoPollingByDefault(t *testing.T) {
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "plain config")
	asserts.Equal(t, len(a.pollRepos), 0, "no poll repos")
	asserts.True(t, a.reactions == nil, "no store allocated when polling is off")
}

func TestPollRepoOnce_DispatchesNewReaction(t *testing.T) {
	api := &fakeAPI{
		comments:  []*gogithub.IssueComment{testComment(9001, 42, 1)},
		reactions: map[int64][]*gogithub.Reaction{9001: {testReaction(1, "+1", 42, "User", time.Now())}},
	}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	a := pollAdapter(t, pollConfig(), srv)

	var mu sync.Mutex
	var got []*core.Reaction
	done := make(chan struct{}, 1)
	err := a.pollRepoOnce(context.Background(), context.Background(),
		reactionCaptureDeps(&mu, &got, done), testRepo,
		map[int64]int{}, map[int64]int{}, time.Now().Add(-time.Hour))
	asserts.NoError(t, err, "poll cycle")
	awaitDispatch(t, done, 1)

	asserts.Equal(t, len(got), 1, "one reaction dispatched")
	r := got[0]
	asserts.Equal(t, r.Emoji, "👍", "content mapped to unicode")
	asserts.Equal(t, r.UserID, "42", "numeric user id, mirroring toMessage")
	asserts.Equal(t, r.AuthorName, "alice", "author login")
	asserts.Equal(t, r.ChannelID, "lao/botbooter#42", "channel from comment issue_url")
	asserts.Equal(t, r.MessageID, "9001", "reacted comment id")
	raw, ok := RawReaction(r)
	asserts.True(t, ok, "RawReaction recovers the payload")
	asserts.Equal(t, raw.Reaction.GetContent(), "+1", "raw content name")
	asserts.Equal(t, raw.Comment.GetID(), int64(9001), "raw comment")
}

func TestToReaction_UnknownContentFallsBack(t *testing.T) {
	r := toReaction(testRepo, 7, testComment(1, 7, 1), testReaction(2, "sparkle", 42, "User", time.Now()))
	asserts.Equal(t, r.Emoji, "sparkle", "unknown content degrades to the raw name")
}

// Reactions from before the cutoff are ignored entirely: not dispatched and —
// so persistent stores stay small — never inserted into the store.
func TestPollRepoOnce_CutoffSkipsAndSparesStore(t *testing.T) {
	api := &fakeAPI{
		comments:  []*gogithub.IssueComment{testComment(9001, 42, 1)},
		reactions: map[int64][]*gogithub.Reaction{9001: {testReaction(1, "+1", 42, "User", time.Now().Add(-time.Hour))}},
	}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	a := pollAdapter(t, pollConfig(), srv)

	var mu sync.Mutex
	var got []*core.Reaction
	err := a.pollRepoOnce(context.Background(), context.Background(),
		reactionCaptureDeps(&mu, &got, nil), testRepo,
		map[int64]int{}, map[int64]int{}, time.Now())
	asserts.NoError(t, err, "poll cycle")
	drainInflight(t, a)

	mu.Lock()
	defer mu.Unlock()
	asserts.Equal(t, len(got), 0, "pre-cutoff reaction not dispatched")
	asserts.Equal(t, storeSize(a), 0, "pre-cutoff reaction never enters the store")
}

func TestPollRepoOnce_SeenReactionNotRedispatched(t *testing.T) {
	api := &fakeAPI{
		comments:  []*gogithub.IssueComment{testComment(9001, 42, 1)},
		reactions: map[int64][]*gogithub.Reaction{9001: {testReaction(1, "+1", 42, "User", time.Now())}},
	}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	a := pollAdapter(t, pollConfig(), srv)

	var mu sync.Mutex
	var got []*core.Reaction
	done := make(chan struct{}, 2)
	deps := reactionCaptureDeps(&mu, &got, done)
	cutoff := time.Now().Add(-time.Hour)

	// Fresh prev/next maps disable the count gate, so only the store dedups.
	asserts.NoError(t, a.pollRepoOnce(context.Background(), context.Background(), deps, testRepo, map[int64]int{}, map[int64]int{}, cutoff), "first cycle")
	awaitDispatch(t, done, 1)
	asserts.NoError(t, a.pollRepoOnce(context.Background(), context.Background(), deps, testRepo, map[int64]int{}, map[int64]int{}, cutoff), "second cycle")
	drainInflight(t, a)

	mu.Lock()
	defer mu.Unlock()
	asserts.Equal(t, len(got), 1, "store dedups across cycles")
}

// An unchanged reactions.total_count must skip the per-comment detail request —
// the property that keeps steady-state cost at one request per repo per cycle.
func TestPollRepoOnce_UnchangedCountSkipsDetailCall(t *testing.T) {
	api := &fakeAPI{
		comments:  []*gogithub.IssueComment{testComment(9001, 42, 1)},
		reactions: map[int64][]*gogithub.Reaction{9001: {testReaction(1, "+1", 42, "User", time.Now())}},
	}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	a := pollAdapter(t, pollConfig(), srv)

	var mu sync.Mutex
	var got []*core.Reaction
	done := make(chan struct{}, 1)
	deps := reactionCaptureDeps(&mu, &got, done)
	cutoff := time.Now().Add(-time.Hour)

	first := map[int64]int{}
	second := map[int64]int{}
	asserts.NoError(t, a.pollRepoOnce(context.Background(), context.Background(), deps, testRepo, map[int64]int{}, first, cutoff), "first cycle")
	awaitDispatch(t, done, 1)
	asserts.NoError(t, a.pollRepoOnce(context.Background(), context.Background(), deps, testRepo, first, second, cutoff), "second cycle")

	_, reactionLists := api.counts()
	asserts.Equal(t, reactionLists, 1, "second cycle skipped the detail request")
	asserts.Equal(t, second[9001], 1, "count carried into the next cycle's cache")
}

func TestPollRepoOnce_BotAndSelfReactorsDropped(t *testing.T) {
	api := &fakeAPI{
		comments: []*gogithub.IssueComment{testComment(9001, 42, 2)},
		reactions: map[int64][]*gogithub.Reaction{9001: {
			testReaction(1, "+1", 555, "Bot", time.Now()),
			testReaction(2, "+1", 777, "User", time.Now()), // the bot's own PAT account
		}},
	}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	a := pollAdapter(t, pollConfig(), srv)
	a.mu.Lock()
	a.selfID = 777
	a.mu.Unlock()

	var mu sync.Mutex
	var got []*core.Reaction
	err := a.pollRepoOnce(context.Background(), context.Background(),
		reactionCaptureDeps(&mu, &got, nil), testRepo,
		map[int64]int{}, map[int64]int{}, time.Now().Add(-time.Hour))
	asserts.NoError(t, err, "poll cycle")
	drainInflight(t, a)

	mu.Lock()
	defer mu.Unlock()
	asserts.Equal(t, len(got), 0, "bot-type and self reactors are dropped")
}

// A comment whose reactions failed to list must be retried next cycle: its
// count enters the cache only after a successful detail fetch.
func TestPollRepoOnce_ErrorLeavesCountUncached(t *testing.T) {
	api := &fakeAPI{comments: []*gogithub.IssueComment{testComment(9001, 42, 1)}}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	a := pollAdapter(t, pollConfig(), srv)

	// No reactions entry: the fake returns an empty list, so break harder by
	// failing the whole API after the comment list is primed in memory.
	api.mu.Lock()
	api.fail = true
	api.mu.Unlock()

	var mu sync.Mutex
	var got []*core.Reaction
	next := map[int64]int{}
	err := a.pollRepoOnce(context.Background(), context.Background(),
		reactionCaptureDeps(&mu, &got, nil), testRepo,
		map[int64]int{}, next, time.Now().Add(-time.Hour))
	asserts.Error(t, err, "listing failure surfaces as a cycle error")
	asserts.Equal(t, len(next), 0, "no count cached for the failed cycle")
}

func TestPollRepoOnce_MalformedIssueURL(t *testing.T) {
	badComment := testComment(9001, 42, 1)
	badComment.IssueURL = gogithub.Ptr("not-a-url")
	api := &fakeAPI{
		comments:  []*gogithub.IssueComment{badComment},
		reactions: map[int64][]*gogithub.Reaction{9001: {testReaction(1, "+1", 42, "User", time.Now())}},
	}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	a := pollAdapter(t, pollConfig(), srv)

	var mu sync.Mutex
	var got []*core.Reaction
	err := a.pollRepoOnce(context.Background(), context.Background(),
		reactionCaptureDeps(&mu, &got, nil), testRepo,
		map[int64]int{}, map[int64]int{}, time.Now().Add(-time.Hour))
	asserts.Error(t, err, "unparseable issue_url is an error, not a silent bad ChannelID")
	drainInflight(t, a)

	mu.Lock()
	defer mu.Unlock()
	asserts.Equal(t, len(got), 0, "nothing dispatched")
}

// A zero-reaction comment — the dominant steady state — must skip the detail
// request even on its first sighting, and still enter the count cache.
func TestPollRepoOnce_ZeroCountSkipsDetailCall(t *testing.T) {
	api := &fakeAPI{comments: []*gogithub.IssueComment{testComment(9001, 42, 0)}}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	a := pollAdapter(t, pollConfig(), srv)

	var mu sync.Mutex
	var got []*core.Reaction
	next := map[int64]int{}
	err := a.pollRepoOnce(context.Background(), context.Background(),
		reactionCaptureDeps(&mu, &got, nil), testRepo,
		map[int64]int{}, next, time.Now().Add(-time.Hour))
	asserts.NoError(t, err, "poll cycle")

	_, reactionLists := api.counts()
	asserts.Equal(t, reactionLists, 0, "no detail request for a zero-count comment")
	cached, ok := next[9001]
	asserts.True(t, ok, "zero count enters the cache")
	asserts.Equal(t, cached, 0, "cached count is zero")
}

// issueNumber must reject non-positive numbers, not just unparseable URLs.
func TestIssueNumber_RejectsNonPositive(t *testing.T) {
	for _, url := range []string{
		"https://api.github.com/repos/lao/botbooter/issues/0",
		"https://api.github.com/repos/lao/botbooter/issues/-1",
	} {
		c := &gogithub.IssueComment{IssueURL: gogithub.Ptr(url)}
		_, err := issueNumber(c)
		asserts.Error(t, err, url)
	}
}

// Reaction dispatch must ride the inflight counter so Disconnect's drain
// covers reaction handlers exactly like webhook dispatch.
func TestPollRepoOnce_DispatchCountsInflight(t *testing.T) {
	api := &fakeAPI{
		comments:  []*gogithub.IssueComment{testComment(9001, 42, 1)},
		reactions: map[int64][]*gogithub.Reaction{9001: {testReaction(1, "+1", 42, "User", time.Now())}},
	}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	a := pollAdapter(t, pollConfig(), srv)

	entered := make(chan struct{})
	release := make(chan struct{})
	deps := core.AdapterDeps{
		DispatchReaction: func(context.Context, *core.Reaction) {
			close(entered)
			<-release
		},
	}
	err := a.pollRepoOnce(context.Background(), context.Background(), deps, testRepo,
		map[int64]int{}, map[int64]int{}, time.Now().Add(-time.Hour))
	asserts.NoError(t, err, "poll cycle")

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("reaction handler never entered")
	}
	asserts.Equal(t, a.inflight.Load(), int64(1), "in-flight reaction dispatch is counted")
	close(release)
	drainCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a.drainDispatch(drainCtx)
	asserts.Equal(t, a.inflight.Load(), int64(0), "counter returns to zero after the handler finishes")
}

// The poll loop must survive API failures — the webhook half keeps serving —
// and stop promptly on ctx cancellation.
func TestPollReactions_SurvivesErrorsAndStopsOnCancel(t *testing.T) {
	api := &fakeAPI{fail: true}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	cfg := pollConfig()
	cfg.ReactionPollInterval = time.Millisecond
	a := pollAdapter(t, cfg, srv)
	a.mu.Lock()
	a.logger = slog.New(slog.NewTextHandler(io.Discard, nil)) // one warn per ms would drown the test output
	a.mu.Unlock()

	deps := core.AdapterDeps{
		DispatchReaction: func(context.Context, *core.Reaction) { t.Error("nothing to dispatch") },
		Done:             func(error) { t.Error("a failing poll must never be fatal") },
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		a.pollReactions(ctx, context.Background(), deps, time.Now())
		close(stopped)
	}()

	// Let it iterate through several failing cycles before canceling.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("poller did not stop on ctx cancellation")
	}
	api.mu.Lock()
	hits := api.hits
	api.mu.Unlock()
	asserts.True(t, hits >= 2, "poller kept retrying across failing cycles")
}

// End-to-end through Connect: the poller starts only after self-identity
// resolves (so cycle one already filters the bot's own reactions), dispatches
// through deps, and dies with the run context.
func TestConnect_StartsReactionPoller(t *testing.T) {
	api := &fakeAPI{
		comments: []*gogithub.IssueComment{testComment(9001, 42, 1)},
		// Created in the future so the fixture cannot predate the connect-time
		// cutoff by the nanoseconds between now and Connect below.
		reactions: map[int64][]*gogithub.Reaction{9001: {testReaction(1, "hooray", 42, "User", time.Now().Add(time.Hour))}},
	}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	cfg := pollConfig()
	cfg.ReactionPollInterval = 5 * time.Millisecond
	a := pollAdapter(t, cfg, srv)

	var mu sync.Mutex
	var got []*core.Reaction
	done := make(chan struct{}, 8)
	deps := reactionCaptureDeps(&mu, &got, done)
	ctx, cancel := context.WithCancel(context.Background())
	asserts.NoError(t, a.Connect(ctx, deps), "connect")
	defer func() { _ = a.Disconnect() }()
	defer cancel()

	awaitDispatch(t, done, 1)
	mu.Lock()
	first := got[0]
	mu.Unlock()
	asserts.Equal(t, first.Emoji, "🎉", "hooray mapped to unicode")
	asserts.Equal(t, first.ChannelID, "lao/botbooter#42", "channel id")
	waitForSelfID(t, a, 777) // poller start implies identity resolved first

	cancel()
	_ = a.Disconnect()
	// After teardown the poller must stop hitting the API.
	settled, _ := api.counts()
	time.Sleep(25 * time.Millisecond)
	after, _ := api.counts()
	asserts.True(t, after <= settled+1, "poller stopped polling after teardown")

	mu.Lock()
	defer mu.Unlock()
	asserts.Equal(t, len(got), 1, "store dedups across the poller's cycles")
}

// Repos listed but no OnReaction handler registered before Connect: the poller
// must not start at all — polling costs API requests every cycle, and nobody
// consumes the dispatches.
func TestConnect_NoReactionHandlersSkipsPoller(t *testing.T) {
	api := &fakeAPI{
		comments:  []*gogithub.IssueComment{testComment(9001, 42, 1)},
		reactions: map[int64][]*gogithub.Reaction{9001: {testReaction(1, "+1", 42, "User", time.Now())}},
	}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	cfg := pollConfig()
	cfg.ReactionPollInterval = time.Millisecond
	a := pollAdapter(t, cfg, srv)

	deps := core.AdapterDeps{
		Dispatch: func(context.Context, *core.Message) {},
		DispatchReaction: func(context.Context, *core.Reaction) {
			t.Error("poller dispatched with no reaction handler registered")
		},
		// HasReactionHandlers deliberately unset: no OnReaction was registered.
		Done:       func(error) {},
		Disconnect: func() error { return nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	asserts.NoError(t, a.Connect(ctx, deps), "connect")
	defer func() { _ = a.Disconnect() }()

	// Identity resolution precedes any poller start; a wrongly-started poller
	// at a 1ms interval would list comments many times over this window.
	waitForSelfID(t, a, 777)
	time.Sleep(20 * time.Millisecond)
	commentLists, _ := api.counts()
	asserts.Equal(t, commentLists, 0, "no poll cycle ran without a reaction handler")
}
