package main

import (
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v88/github"
)

func prEvent(action, repo, login string, number int) *gogithub.PullRequestEvent {
	return &gogithub.PullRequestEvent{
		Action: gogithub.Ptr(action),
		Repo:   &gogithub.Repository{FullName: gogithub.Ptr(repo)},
		PullRequest: &gogithub.PullRequest{
			Number: gogithub.Ptr(number),
			User:   &gogithub.User{Login: gogithub.Ptr(login)},
		},
	}
}

// welcomeForEvent decides which pull_request deliveries draw a welcome
// comment. The adapter forwards opened, reopened and synchronize actions, but
// only "opened" is a PR's first appearance — welcoming the others would spam
// the thread on every push.
func TestWelcomeForEvent(t *testing.T) {
	t.Run("OpenedIsWelcomed", func(t *testing.T) {
		channel, comment, ok := welcomeForEvent(prEvent("opened", "lao/botbooter", "alice", 7))
		if !ok {
			t.Fatal("welcomeForEvent(opened) not ok, want welcome")
		}
		if channel != "lao/botbooter#7" {
			t.Errorf("channel = %q, want %q", channel, "lao/botbooter#7")
		}
		if !strings.Contains(comment, "@alice") {
			t.Errorf("comment %q does not mention @alice", comment)
		}
	})

	for _, action := range []string{"reopened", "synchronize"} {
		t.Run(action+"IsSkipped", func(t *testing.T) {
			if _, _, ok := welcomeForEvent(prEvent(action, "lao/botbooter", "alice", 7)); ok {
				t.Errorf("welcomeForEvent(%s) ok, want skip", action)
			}
		})
	}

	t.Run("DegeneratePayloadIsSkipped", func(t *testing.T) {
		if _, _, ok := welcomeForEvent(&gogithub.PullRequestEvent{Action: gogithub.Ptr("opened")}); ok {
			t.Error("welcomeForEvent(opened, empty payload) ok, want skip")
		}
	})
}
