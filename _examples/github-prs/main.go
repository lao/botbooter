// Command github-prs demonstrates reacting when a pull request is opened: the
// bot posts a welcome comment on every new PR. GITHUB_PR_MODE picks how PR
// creation is observed — "poll" (the default) or "webhook".
//
//	go run ./_examples/github-prs   # reads GITHUB_TOKEN (or GITHUB_APP_ID / GITHUB_INSTALLATION_ID / GITHUB_PRIVATE_KEY_FILE) (and optional GITHUB_PR_MODE, GITHUB_REPO, GITHUB_PR_POLL_SECONDS, GITHUB_WEBHOOK_SECRET, GITHUB_ADDR, GITHUB_PATH)
//
// # Webhook mode
//
// GITHUB_PR_MODE=webhook uses the adapter's pull_request ingress
// (github.Config.OnPullRequest): GitHub pushes each delivery to the bot's
// webhook endpoint, so a new PR is welcomed instantly, with no polling budget
// and no eventual-consistency window. The price is reachability: the endpoint
// (GITHUB_ADDR, default ":8080") must be exposed at a public HTTPS URL and
// registered as a repository or App webhook subscribed to the pull_request
// event (add issue_comment for the echo command below), with
// GITHUB_WEBHOOK_SECRET matching the registered secret — required in this
// mode, because GitHub signs every delivery. The adapter already drops
// bot-authored and self-authored PRs and forwards only the opened, reopened
// and synchronize actions; this example welcomes only "opened", a PR's first
// appearance. GITHUB_REPO is ignored — the webhook registration itself decides
// which repositories deliver.
//
// # Poll mode
//
// Poll mode needs no public URL and no webhook registration: a single watcher
// polls the Search API for freshly created PRs through the bot's own
// authenticated API client (github.Client). Only the API credentials are
// required. GITHUB_REPO ("owner/name") pins the watch to one repository, and
// the wildcard form ("owner/*") narrows the watch to that owner's
// repositories among the discovered set; when unset the example watches every
// repository the credentials can reach, discovered once at startup — the App
// installation's granted repos in App mode (GET /installation/repositories),
// everything the token user can access in PAT mode (GET /user/repos; a
// fine-grained PAT narrows this to its granted repos, a classic PAT sees
// every repo the account sees) — skipping archived ones, which cannot receive
// new PRs.
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
// In poll mode the webhook half of the adapter still needs an address to
// bind, but PR watching never uses it, so it defaults to a localhost-only
// listener with a placeholder secret; set GITHUB_WEBHOOK_SECRET/GITHUB_ADDR
// and register the URL as a repo webhook (events: issue_comment) only if you
// also want the "echo" command to answer comments on the PR.
//
// Either way the reply goes through the bot's normal egress —
// bot.SendMessageContext with channel "owner/repo#number" posts an issue
// comment, and PRs are issues for commenting purposes, so the comment lands
// on the PR conversation.
//
// The code is split by concern: main.go (env parsing, wiring), bot.go
// (adapter construction, the welcome comment), webhook.go (webhook mode),
// scope.go (poll-mode repository discovery), poll.go (the Search API
// watcher).
package main

import (
	"cmp"
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/lao/botbooter"
)

func main() {
	_ = godotenv.Load(".env")

	var bot *botbooter.Bot
	var err error
	mode := cmp.Or(os.Getenv("GITHUB_PR_MODE"), "poll")
	switch mode {
	case "webhook":
		bot, err = newWebhookBot()
	case "poll":
		bot, err = newBot(nil)
	default:
		log.Fatalf(`GITHUB_PR_MODE must be "poll" or "webhook", got %q`, mode)
	}
	if err != nil {
		log.Fatal(err)
	}

	// The webhook half also answers comments: commenting "echo <text>" on the
	// welcomed PR echoes back, proving both ingress directions on one PR. In
	// poll mode this needs the issue_comment webhook registered; in webhook
	// mode the endpoint is already registered, so just add the event.
	bot.HandleFunc("^echo ", func(ctx context.Context, b *botbooter.Bot, message *botbooter.Message) {
		reply := "You said: " + strings.TrimPrefix(message.Content, "echo ")
		if err := b.SendMessageContext(ctx, message.ChannelID, reply); err != nil {
			log.Printf("failed to send: %v", err)
		}
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if mode == "webhook" {
		log.Printf("serving pull_request webhooks on %s (register the public URL with events: pull_request, issue_comment)",
			cmp.Or(os.Getenv("GITHUB_ADDR"), defaultWebhookAddr))
	} else if err := startPollWatcher(ctx, bot); err != nil {
		log.Fatal(err)
	}

	if err := bot.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
