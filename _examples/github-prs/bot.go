package main

import (
	"cmp"
	"fmt"
	"os"
	"strconv"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/github"
)

// welcomeComment is the comment posted on a freshly opened PR, shared by both
// ingress modes so switching modes doesn't change what contributors see.
func welcomeComment(login string) string {
	return fmt.Sprintf("👋 Thanks for opening this pull request, @%s! Someone will review it soon. (Reply `echo <text>` to test the comment bot.)", login)
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
