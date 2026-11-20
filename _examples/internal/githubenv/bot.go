// Package githubenv loads GitHub credentials for the example commands.
package githubenv

import (
	"fmt"
	"os"
	"strconv"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/github"
)

// NewBot adds the token or App credentials from the environment to cfg.
func NewBot(cfg github.Config) (*botbooter.Bot, error) {
	cfg.Token = os.Getenv("GITHUB_TOKEN")
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
