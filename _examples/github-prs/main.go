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
//
// The code is split by concern: main.go (env parsing, wiring), bot.go
// (adapter construction, the welcome comment), scope.go (repository
// discovery), poll.go (the Search API watcher).
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/lao/botbooter"
)

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
