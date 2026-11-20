package exampletest

import (
	"path/filepath"
	"testing"

	"github.com/lao/botbooter"
)

// CheckPlatforms constructs each supported adapter using local test settings.
func CheckPlatforms(t *testing.T, newBot func(string) (*botbooter.Bot, error), platforms []string) {
	t.Helper()
	for key, value := range map[string]string{
		"SLACK_APP_TOKEN": "xapp-test", "SLACK_BOT_TOKEN": "xoxb-test", "DISCORD_BOT_TOKEN": "test",
		"TELEGRAM_BOT_TOKEN": "123456:test-token",
		"WA_TOKEN":           "test", "WA_PHONE_ID": "1", "WA_APP_SECRET": "secret", "WA_VERIFY_TOKEN": "verify", "WA_ADDR": ":0",
		"TEAMS_APP_ID": "app", "TEAMS_APP_PASSWORD": "secret", "TEAMS_ADDR": ":0",
		"GITHUB_TOKEN": "token", "GITHUB_PRIVATE_KEY_FILE": "", "GITHUB_WEBHOOK_SECRET": "secret", "GITHUB_ADDR": ":0",
		"GITHUB_REPO": "owner/repo", "GITHUB_POLL_SECONDS": "30", "GITHUB_POLL_AUTO_INTERVAL": "",
		"GITLAB_TOKEN": "token", "GITLAB_SECRET": "secret", "GITLAB_ADDR": ":0", "GITLAB_BASE_URL": "",
		"BITBUCKET_EMAIL": "example@example.org", "BITBUCKET_API_TOKEN": "token", "BITBUCKET_ACCESS_TOKEN": "",
		"BITBUCKET_SECRET": "secret", "BITBUCKET_ADDR": ":0", "BITBUCKET_BASE_URL": "",
		"SIGNAL_API_URL": "http://127.0.0.1", "SIGNAL_NUMBER": "+15555550100",
	} {
		t.Setenv(key, value)
	}
	for _, name := range platforms {
		t.Run(name, func(t *testing.T) {
			bot, err := newBot(name)
			if err != nil {
				t.Fatal(err)
			}
			if got := bot.BotType.String(); got != name {
				t.Fatalf("selected %s for %s", got, name)
			}
		})
	}
	t.Setenv("WA_MEOW_DB", filepath.Join(t.TempDir(), "missing", "session.db"))
	if _, err := newBot("whatsmeow"); err == nil {
		t.Error("accepted a session path with a missing parent")
	}
	if _, err := newBot("unknown"); err == nil {
		t.Error("accepted an unknown platform")
	}
}
