package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	gogithub "github.com/google/go-github/v88/github"
	"github.com/lao/botbooter"
	"github.com/lao/botbooter/github"
)

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
		Token:         os.Getenv("GITHUB_TOKEN"),
		WebhookSecret: os.Getenv("GITHUB_WEBHOOK_SECRET"),
		Addr:          os.Getenv("GITHUB_ADDR"),
		Path:          os.Getenv("GITHUB_PATH"), // optional; defaults to /webhook
		OnPullRequest: onPullRequest,
	}
	if onPullRequest != nil {
		if cfg.WebhookSecret == "" {
			return nil, errors.New("GITHUB_WEBHOOK_SECRET is required in webhook mode: it must match the secret registered on the repository webhook")
		}
		cfg.Addr = cmp.Or(cfg.Addr, ":8080")
	} else {
		cfg.WebhookSecret = cmp.Or(cfg.WebhookSecret, "github-prs-example-unused")
		cfg.Addr = cmp.Or(cfg.Addr, "127.0.0.1:0")
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
