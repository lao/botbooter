package main

import (
	"context"
	"log"
	"strconv"

	gogithub "github.com/google/go-github/v88/github"
	"github.com/lao/botbooter"
)

// welcomeForEvent maps a pull_request delivery to the welcome it should draw:
// the "owner/repo#number" channel and the comment text. The adapter forwards
// opened, reopened and synchronize actions, but only "opened" is a PR's first
// appearance — welcoming the others would spam the thread on every push. Bot
// and self authors never get here; the adapter already dropped them.
func welcomeForEvent(event *gogithub.PullRequestEvent) (channel, comment string, ok bool) {
	repo := event.GetRepo().GetFullName()
	number := event.GetPullRequest().GetNumber()
	if event.GetAction() != "opened" || repo == "" || number == 0 {
		return "", "", false
	}
	return repo + "#" + strconv.Itoa(number), welcomeComment(event.GetPullRequest().GetUser().GetLogin()), true
}

// newWebhookBot builds the bot with an OnPullRequest callback that welcomes
// freshly opened PRs. The callback closes over the bot variable assigned right
// after construction — safe because the adapter only delivers webhooks after
// Connect, long after New returned.
func newWebhookBot() (*botbooter.Bot, error) {
	var bot *botbooter.Bot
	var err error
	bot, err = newBot(func(ctx context.Context, event *gogithub.PullRequestEvent) {
		channel, comment, ok := welcomeForEvent(event)
		if !ok {
			return
		}
		log.Printf("new PR %s by %s: %q",
			channel, event.GetPullRequest().GetUser().GetLogin(), event.GetPullRequest().GetTitle())
		if err := bot.SendMessageContext(ctx, channel, comment); err != nil {
			log.Printf("failed to welcome PR %s: %v", channel, err)
		}
	})
	return bot, err
}
