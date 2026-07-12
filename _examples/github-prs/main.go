// Command github-prs demonstrates reacting when a pull request is opened on a
// GitHub repository: the bot posts a welcome comment on every new PR.
//
// The adapter's webhook ingress is issue_comment events only — a pull_request
// "opened" delivery is acked and dropped, so no command handler fires when a PR
// is created. This example works around that gap entirely with the public API,
// the same way reaction ingress was prototyped before it moved into the
// adapter: a small poller lists the repo's newest pull requests through the
// bot's own authenticated API client (github.Client) and replies through the
// bot's normal egress — bot.SendMessageContext with channel "owner/repo#number"
// posts an issue comment, and PRs are issues for commenting purposes, so the
// comment lands on the PR conversation.
//
//	go run ./_examples/github-prs   # reads GITHUB_TOKEN (or GITHUB_APP_ID / GITHUB_INSTALLATION_ID / GITHUB_PRIVATE_KEY_FILE) (and optional GITHUB_REPO, GITHUB_PR_POLL_SECONDS, GITHUB_WEBHOOK_SECRET, GITHUB_ADDR, GITHUB_PATH)
//
// Only the API credentials are required. GITHUB_REPO ("owner/name") pins the
// watch to one repository; when unset the example discovers every repository
// the credentials can reach at startup — the App installation's granted repos
// in App mode (GET /installation/repositories), everything the token user can
// access in PAT mode (GET /user/repos; a fine-grained PAT narrows this to its
// granted repos, a classic PAT sees every repo the account sees) — skipping
// archived ones, which cannot receive new PRs. Watching costs one API call per
// repository per cycle, so the startup log prints the projected calls/hour;
// against a PAT's 5000 req/h budget that caps out around 40 repos at the 30s
// default — raise GITHUB_PR_POLL_SECONDS or pin GITHUB_REPO if discovery finds
// more. The
// webhook half of the adapter still needs an address to bind, but PR watching
// never uses it, so it defaults to a localhost-only listener with a placeholder
// secret; set GITHUB_WEBHOOK_SECRET/GITHUB_ADDR and register the URL as a repo
// webhook (events: issue_comment) only if you also want the "echo" command to
// answer comments on the PR.
//
// Watching is polling, not push: one PR-list API call per cycle
// (GITHUB_PR_POLL_SECONDS, default 30), well inside a PAT's 5000 req/h budget,
// and only PRs opened while the example is running are welcomed. Bot-authored
// PRs (User.Type "Bot", which covers this bot in App mode) are skipped so two
// bots cannot ping-pong; in PAT mode the token user's own PRs arrive as a plain
// User and are NOT skipped — don't open PRs with the same account the bot posts
// with.
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

// repoRef names one repository to watch.
type repoRef struct{ owner, name string }

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

	repos, err := reposToWatch(ctx, bot)
	if err != nil {
		log.Fatal(err)
	}
	if len(repos) == 0 {
		log.Fatal("no repositories to watch: the credentials reach none (grant the App/PAT access, or set GITHUB_REPO)")
	}

	for _, r := range repos {
		go watchPullRequests(ctx, bot, r.owner, r.name, interval)
	}

	calls := int64(len(repos)) * int64(time.Hour/interval)
	log.Printf("watching %d repositories for new pull requests every %s (~%d API calls/hour)",
		len(repos), interval, calls)
	if calls > 4000 {
		log.Printf("warning: ~%d calls/hour risks the 5000/h API budget; raise GITHUB_PR_POLL_SECONDS or pin GITHUB_REPO", calls)
	}
	if err := bot.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

// reposToWatch returns the explicit GITHUB_REPO when set, and otherwise
// discovers every repository the credentials can reach: the installation's
// granted repos in App mode, the token user's accessible repos in PAT mode.
// Archived repositories are skipped — they cannot receive new PRs, so watching
// them only burns API budget.
func reposToWatch(ctx context.Context, bot *botbooter.Bot) ([]repoRef, error) {
	if repo := os.Getenv("GITHUB_REPO"); repo != "" {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" {
			return nil, fmt.Errorf(`GITHUB_REPO must be "owner/name", got %q`, repo)
		}
		return []repoRef{{owner, name}}, nil
	}

	client := github.Client(bot)
	appMode := os.Getenv("GITHUB_PRIVATE_KEY_FILE") != ""
	var out []repoRef
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
			return nil, fmt.Errorf("discover repositories: %w", err)
		}
		for _, r := range repos {
			if r.GetArchived() {
				continue
			}
			out = append(out, repoRef{r.GetOwner().GetLogin(), r.GetName()})
			log.Printf("discovered %s", r.GetFullName())
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		page = resp.NextPage
	}
}

// watchPullRequests polls the repo's newest open PRs and welcomes each one
// created after startup, exactly once. It reads through the bot's own API
// client and replies through the bot's Send path, so auth (PAT or App) and
// rate-limit handling stay in one place.
func watchPullRequests(ctx context.Context, bot *botbooter.Bot, owner, name string, every time.Duration) {
	client := github.Client(bot)
	since := time.Now()
	welcomed := map[int]bool{}

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Newest-first with a small page: one API call per cycle, and a PR
		// older than `since` ends the scan for this page.
		prs, _, err := client.PullRequests.List(ctx, owner, name, &gogithub.PullRequestListOptions{
			State:       "open",
			Sort:        "created",
			Direction:   "desc",
			ListOptions: gogithub.ListOptions{PerPage: 10},
		})
		if err != nil {
			log.Printf("list pull requests: %v", err)
			continue
		}

		for _, pr := range prs {
			if pr.GetCreatedAt().Time.Before(since) {
				break
			}
			num := pr.GetNumber()
			if welcomed[num] || pr.GetUser().GetType() == "Bot" {
				continue
			}
			welcomed[num] = true

			log.Printf("new PR #%d by %s: %q", num, pr.GetUser().GetLogin(), pr.GetTitle())
			channel := owner + "/" + name + "#" + strconv.Itoa(num)
			welcome := fmt.Sprintf("👋 Thanks for opening this pull request, @%s! Someone will review it soon. (Reply `echo <text>` to test the comment bot.)", pr.GetUser().GetLogin())
			if err := bot.SendMessageContext(ctx, channel, welcome); err != nil {
				log.Printf("failed to welcome PR #%d: %v", num, err)
				delete(welcomed, num) // retry next cycle
			}
		}
	}
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
