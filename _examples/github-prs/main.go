// Command github-prs demonstrates reacting when a pull request is opened on
// any repository the bot can reach: the bot posts a welcome comment on every
// new PR.
//
// The adapter's webhook ingress is issue_comment events only — a pull_request
// "opened" delivery is acked and dropped, so no command handler fires when a PR
// is created. This example works around that gap entirely with the public API,
// the same way reaction ingress was prototyped before it moved into the
// adapter: a single watcher polls the Search API for freshly created PRs
// through the bot's own authenticated API client (github.Client) and replies
// through the bot's normal egress — bot.SendMessageContext with channel
// "owner/repo#number" posts an issue comment, and PRs are issues for
// commenting purposes, so the comment lands on the PR conversation.
//
//	go run ./_examples/github-prs   # reads GITHUB_TOKEN (or GITHUB_APP_ID / GITHUB_INSTALLATION_ID / GITHUB_PRIVATE_KEY_FILE) (and optional GITHUB_REPO, GITHUB_PR_POLL_SECONDS, GITHUB_WEBHOOK_SECRET, GITHUB_ADDR, GITHUB_PATH)
//
// Only the API credentials are required. GITHUB_REPO ("owner/name") pins the
// watch to one repository, and the wildcard form ("owner/*") narrows the watch
// to that owner's repositories among the discovered set; when unset the
// example watches every repository the credentials can reach, discovered once
// at startup — the App installation's
// granted repos in App mode (GET /installation/repositories), everything the
// token user can access in PAT mode (GET /user/repos; a fine-grained PAT
// narrows this to its granted repos, a classic PAT sees every repo the account
// sees) — skipping archived ones, which cannot receive new PRs.
//
// The watch itself is one Search API query per cycle regardless of repo count
// (GITHUB_PR_POLL_SECONDS, default 30), paginated only when a cycle's window
// overflows one page: "is:pr created:>=<cutoff>" scoped by user:/org:
// qualifiers built from the discovered owners. Search tokens are
// NOT scoped to what the credentials can reach (an installation token searches
// all of GitHub), so results are also filtered against the discovered repo
// set; that filter is what keeps a user:-qualifier match on a non-granted repo
// from drawing a doomed comment attempt. Search is a separate rate pool (30
// requests/min) with plenty of headroom at the default interval; owner lists
// long enough to need many query chunks get a startup warning. The search
// index is eventually consistent, so each cycle re-queries a two-minute
// overlap window and a seen-set drops duplicates; only PRs created after
// startup are welcomed.
//
// Bot-authored PRs (User.Type "Bot", which covers this bot in App mode) are
// skipped so two bots cannot ping-pong; in PAT mode the token user's own PRs
// arrive as a plain User and are NOT skipped — don't open PRs with the same
// account the bot posts with.
//
// The webhook half of the adapter still needs an address to bind, but PR
// watching never uses it, so it defaults to a localhost-only listener with a
// placeholder secret; set GITHUB_WEBHOOK_SECRET/GITHUB_ADDR and register the
// URL as a repo webhook (events: issue_comment) only if you also want the
// "echo" command to answer comments on the PR.
package main

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	gogithub "github.com/google/go-github/v88/github"
	"github.com/joho/godotenv"
	"github.com/lao/botbooter"
	"github.com/lao/botbooter/github"
)

// searchLag is the re-query overlap compensating for the Search API's
// eventually consistent index; the seen-set dedupes within it.
const searchLag = 2 * time.Minute

// maxQualifierChars caps one query's owner qualifiers, leaving room for the
// fixed "is:pr created:>=..." prefix inside GitHub's 256-char query limit.
const maxQualifierChars = 200

// watchScope is where the watcher looks: qualifier strings that scope Search
// API queries ("repo:owner/name", or chunked "user:a org:b ..."), and the
// exact repositories the credentials can reach — search qualifiers over-match
// (user:X covers all of X's repos, granted or not), so hits are filtered
// against allowed before replying.
type watchScope struct {
	qualifiers []string
	allowed    map[string]bool // keyed "owner/name"
}

func main() {
	_ = godotenv.Load(".env")

	interval := 30 * time.Second
	if s := os.Getenv("GITHUB_PR_POLL_SECONDS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			log.Fatalf("parse GITHUB_PR_POLL_SECONDS %q", s)
		}
		interval = time.Duration(n) * time.Second
	}

	bot, err := newBot()
	if err != nil {
		log.Fatal(err)
	}

	// The webhook half still works when registered: commenting "echo <text>" on
	// the welcomed PR answers back, proving both ingress directions on one PR.
	bot.HandleFunc("^echo ", func(ctx context.Context, b *botbooter.Bot, message *botbooter.Message) {
		reply := "You said: " + strings.TrimPrefix(message.Content, "echo ")
		if err := b.SendMessageContext(ctx, message.ChannelID, reply); err != nil {
			log.Printf("failed to send: %v", err)
		}
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Bound discovery so a stalled network call fails the startup instead of
	// hanging it; the watch loop bounds its own cycles the same way.
	discCtx, discCancel := context.WithTimeout(ctx, time.Minute)
	scope, err := resolveScope(discCtx, bot)
	discCancel()
	if err != nil {
		log.Fatal(err)
	}
	if len(scope.allowed) == 0 {
		log.Fatal("no repositories to watch: the credentials reach none (grant the App/PAT access, or set GITHUB_REPO)")
	}

	go watchPullRequests(ctx, bot, scope, interval)

	log.Printf("watching %d repositories for new pull requests every %s (%d search query/cycle)",
		len(scope.allowed), interval, len(scope.qualifiers))
	if perMin := len(scope.qualifiers) * int(time.Minute/interval); perMin > 25 {
		log.Printf("warning: ~%d searches/min risks the 30/min Search API budget; raise GITHUB_PR_POLL_SECONDS", perMin)
	}
	if err := bot.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

// watchPullRequests polls the Search API for PRs created since the last cycle
// across the whole scope at once and welcomes each exactly once. It reads
// through the bot's own API client and replies through the bot's Send path, so
// auth (PAT or App) and rate-limit handling stay in one place.
func watchPullRequests(ctx context.Context, bot *botbooter.Bot, scope watchScope, every time.Duration) {
	client := github.Client(bot)
	since := time.Now()
	welcomed := map[string]bool{} // keyed "owner/name#number", the channel ID

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Overlap the previous window by searchLag, but never reach back past
		// startup: pre-existing PRs are not this watcher's to welcome.
		cutoff := time.Now().Add(-searchLag)
		if cutoff.Before(since) {
			cutoff = since
		}
		// Bound the cycle by the poll interval so one stalled network call
		// cannot block the loop indefinitely; whatever it cut off is re-found
		// next cycle through the searchLag overlap.
		cycleCtx, cancel := context.WithTimeout(ctx, every)
		for _, qualifier := range scope.qualifiers {
			query := fmt.Sprintf("is:pr created:>=%s %s", cutoff.UTC().Format("2006-01-02T15:04:05Z"), qualifier)
			opts := &gogithub.SearchOptions{
				Sort:        "created",
				Order:       "desc",
				ListOptions: gogithub.ListOptions{PerPage: 50},
			}
			for {
				res, resp, err := client.Search.Issues(cycleCtx, query, opts)
				if err != nil {
					log.Printf("search pull requests: %v", err)
					break
				}

				for _, pr := range res.Issues {
					repo := strings.TrimPrefix(pr.GetRepositoryURL(), "https://api.github.com/repos/")
					channel := repo + "#" + strconv.Itoa(pr.GetNumber())
					if !scope.allowed[repo] || welcomed[channel] || pr.GetUser().GetType() == "Bot" {
						continue
					}
					welcomed[channel] = true

					log.Printf("new PR %s by %s: %q", channel, pr.GetUser().GetLogin(), pr.GetTitle())
					welcome := fmt.Sprintf("👋 Thanks for opening this pull request, @%s! Someone will review it soon. (Reply `echo <text>` to test the comment bot.)", pr.GetUser().GetLogin())
					if err := bot.SendMessageContext(cycleCtx, channel, welcome); err != nil {
						log.Printf("failed to welcome PR %s: %v", channel, err)
						delete(welcomed, channel) // retry next cycle
					}
				}
				if resp.NextPage == 0 {
					break
				}
				opts.Page = resp.NextPage
			}
		}
		cancel()
	}
}

// resolveScope returns the explicit GITHUB_REPO scope when set, and otherwise
// discovers every repository the credentials can reach: the installation's
// granted repos in App mode, the token user's accessible repos in PAT mode.
// The wildcard "owner/*" (matching the adapter's ReactionPollRepos form — "*"
// is only special as the whole name) runs the same discovery narrowed to that
// owner, so it still only covers repos the credentials reach. Archived
// repositories are skipped — they cannot receive new PRs. Owners dedupe into
// user:/org: search qualifiers, chunked under the query length cap.
func resolveScope(ctx context.Context, bot *botbooter.Bot) (watchScope, error) {
	ownerFilter := ""
	if repo := os.Getenv("GITHUB_REPO"); repo != "" {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" {
			return watchScope{}, fmt.Errorf(`GITHUB_REPO must be "owner/name" or "owner/*", got %q`, repo)
		}
		if name != "*" {
			return watchScope{
				qualifiers: []string{"repo:" + repo},
				allowed:    map[string]bool{repo: true},
			}, nil
		}
		ownerFilter = owner
	}

	client := github.Client(bot)
	appMode := os.Getenv("GITHUB_PRIVATE_KEY_FILE") != ""
	scope := watchScope{allowed: map[string]bool{}}
	var owners []string // insertion-ordered for stable qualifiers
	seenOwner := map[string]bool{}
	page := 1
	for {
		var repos []*gogithub.Repository
		var resp *gogithub.Response
		var err error
		if appMode {
			var lst *gogithub.ListRepositories
			lst, resp, err = client.Apps.ListRepos(ctx, &gogithub.ListOptions{PerPage: 100, Page: page})
			if lst != nil {
				repos = lst.Repositories
			}
		} else {
			repos, resp, err = client.Repositories.ListByAuthenticatedUser(ctx,
				&gogithub.RepositoryListByAuthenticatedUserOptions{ListOptions: gogithub.ListOptions{PerPage: 100, Page: page}})
		}
		if err != nil {
			return watchScope{}, fmt.Errorf("discover repositories: %w", err)
		}
		for _, r := range repos {
			if r.GetArchived() {
				continue
			}
			// GitHub owner names are case-insensitive; match the wildcard the
			// same way so a casing mismatch doesn't silently watch nothing.
			if ownerFilter != "" && !strings.EqualFold(r.GetOwner().GetLogin(), ownerFilter) {
				continue
			}
			scope.allowed[r.GetFullName()] = true
			log.Printf("discovered %s", r.GetFullName())
			qualifier := "user:" + r.GetOwner().GetLogin()
			if r.GetOwner().GetType() == "Organization" {
				qualifier = "org:" + r.GetOwner().GetLogin()
			}
			if !seenOwner[qualifier] {
				seenOwner[qualifier] = true
				owners = append(owners, qualifier)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		page = resp.NextPage
	}
	scope.qualifiers = chunkQualifiers(owners)
	return scope, nil
}

// chunkQualifiers space-joins owner qualifiers into as few search queries as
// fit the query length cap; qualifiers of one kind OR together in GitHub
// search, so each chunk is a single query.
func chunkQualifiers(owners []string) []string {
	var chunks []string
	current := ""
	for _, o := range owners {
		switch {
		case current == "":
			current = o
		case len(current)+1+len(o) > maxQualifierChars:
			chunks = append(chunks, current)
			current = o
		default:
			current += " " + o
		}
	}
	if current != "" {
		chunks = append(chunks, current)
	}
	return chunks
}

func newBot() (*botbooter.Bot, error) {
	cfg := github.Config{
		Token: os.Getenv("GITHUB_TOKEN"),
		// PR watching never receives a webhook, but the adapter requires the
		// webhook half to be configured; default to a localhost-only listener
		// with a placeholder secret so the example runs with credentials alone.
		WebhookSecret: cmp.Or(os.Getenv("GITHUB_WEBHOOK_SECRET"), "github-prs-example-unused"),
		Addr:          cmp.Or(os.Getenv("GITHUB_ADDR"), "127.0.0.1:0"),
		Path:          os.Getenv("GITHUB_PATH"), // optional; defaults to /webhook
	}
	if keyFile := os.Getenv("GITHUB_PRIVATE_KEY_FILE"); keyFile != "" {
		key, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("read GITHUB_PRIVATE_KEY_FILE: %w", err)
		}
		appID, err := strconv.ParseInt(os.Getenv("GITHUB_APP_ID"), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse GITHUB_APP_ID: %w", err)
		}
		installationID, err := strconv.ParseInt(os.Getenv("GITHUB_INSTALLATION_ID"), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse GITHUB_INSTALLATION_ID: %w", err)
		}
		cfg.AppID, cfg.InstallationID, cfg.PrivateKey = appID, installationID, key
	}
	return github.New(cfg)
}
