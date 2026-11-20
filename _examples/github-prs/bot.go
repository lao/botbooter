package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"

	gogithub "github.com/google/go-github/v88/github"
	"github.com/lao/botbooter"
	"github.com/lao/botbooter/_examples/internal/githubenv"
	"github.com/lao/botbooter/github"
)

// defaultWebhookAddr is where webhook mode binds when GITHUB_ADDR is unset.
const defaultWebhookAddr = ":8080"

// welcomeComment is the comment posted on a freshly opened PR, shared by both
// ingress modes so switching modes doesn't change what contributors see.
func welcomeComment(login string) string {
	return fmt.Sprintf("👋 Thanks for opening this pull request, @%s! Someone will review it soon. (Reply `echo <text>` to test the comment bot.)", login)
}

// newBot builds the GitHub bot from the environment. A non-nil onPullRequest
// selects webhook mode: the callback is wired into the adapter, the webhook
// endpoint binds a real address (GITHUB_ADDR, default ":8080") and
// GITHUB_WEBHOOK_SECRET becomes required — a placeholder secret would make
// GitHub's signed deliveries fail verification silently. Nil keeps poll mode,
// where the webhook half is dead weight: a localhost-only listener with a
// placeholder secret, so the example runs with credentials alone.
func newBot(onPullRequest func(context.Context, *gogithub.PullRequestEvent)) (*botbooter.Bot, error) {
	cfg := github.Config{
		WebhookSecret: os.Getenv("GITHUB_WEBHOOK_SECRET"),
		Addr:          os.Getenv("GITHUB_ADDR"),
		Path:          os.Getenv("GITHUB_PATH"), // optional; defaults to /webhook
		OnPullRequest: onPullRequest,
	}
	if onPullRequest != nil {
		if cfg.WebhookSecret == "" {
			return nil, errors.New("GITHUB_WEBHOOK_SECRET is required in webhook mode: it must match the secret registered on the repository webhook")
		}
		cfg.Addr = cmp.Or(cfg.Addr, defaultWebhookAddr)
	} else {
		cfg.WebhookSecret = cmp.Or(cfg.WebhookSecret, "github-prs-example-unused")
		cfg.Addr = cmp.Or(cfg.Addr, "127.0.0.1:0")
	}
	return githubenv.NewBot(cfg)
}
