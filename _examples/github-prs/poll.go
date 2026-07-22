package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v88/github"
	"github.com/lao/botbooter"
	"github.com/lao/botbooter/github"
)

// searchLag is the re-query overlap compensating for the Search API's
// eventually consistent index; the seen-set dedupes within it.
const searchLag = 2 * time.Minute

// startPollWatcher resolves the watch scope and starts the background Search
// API watcher. It returns an error instead of logging fatally so main owns
// process exit.
func startPollWatcher(ctx context.Context, bot *botbooter.Bot) error {
	interval := 30 * time.Second
	if s := os.Getenv("GITHUB_PR_POLL_SECONDS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return fmt.Errorf("parse GITHUB_PR_POLL_SECONDS %q", s)
		}
		interval = time.Duration(n) * time.Second
	}

	// Bound discovery so a stalled network call fails the startup instead of
	// hanging it; the watch loop bounds its own cycles the same way.
	discCtx, discCancel := context.WithTimeout(ctx, time.Minute)
	scope, err := resolveScope(discCtx, bot)
	discCancel()
	if err != nil {
		return err
	}
	if len(scope.allowed) == 0 {
		return fmt.Errorf("no repositories to watch: the credentials reach none (grant the App/PAT access, or set GITHUB_REPO)")
	}

	go watchPullRequests(ctx, bot, scope, interval)

	log.Printf("watching %d repositories for new pull requests every %s (%d search query/cycle)",
		len(scope.allowed), interval, len(scope.qualifiers))
	if perMin := len(scope.qualifiers) * int(time.Minute/interval); perMin > 25 {
		log.Printf("warning: ~%d searches/min risks the 30/min Search API budget; raise GITHUB_PR_POLL_SECONDS", perMin)
	}
	return nil
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
					if err := bot.SendMessageContext(cycleCtx, channel, welcomeComment(pr.GetUser().GetLogin())); err != nil {
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
