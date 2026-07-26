package botbooter_test

import (
	"context"
	"os"
	"strings"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/cli"
)

func Example() {
	bot := cli.New(strings.NewReader("echo hello\n"), os.Stdout)

	bot.HandleFunc("^echo ", func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message) {
		_ = b.SendMessageContext(ctx, m.ChannelID, strings.TrimPrefix(m.Content, "echo "))
	})

	// Run returns when the input reaches EOF.
	_ = bot.Run(context.Background())
	// Output: hello
}

// Example_reply threads a reply onto the triggering message. On the CLI adapter
// (which has no real threading) it degrades to a plain send — b.Reply is the
// convenience form of SendMessageContext(ctx, m.ChannelID, text, InReplyTo(m)).
func Example_reply() {
	bot := cli.New(strings.NewReader("echo hi\n"), os.Stdout)

	bot.HandleFunc("^echo ", func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message) {
		_ = b.Reply(ctx, m, "You said: "+strings.TrimPrefix(m.Content, "echo "))
	})

	_ = bot.Run(context.Background())
	// Output: You said: hi
}

// Example_conversationalFlow runs a multi-step dialog: a matching message starts
// the flow, then each later message from that conversation is routed to the
// active flow until it completes. The trigger message emits the first prompt.
func Example_conversationalFlow() {
	bot := cli.New(strings.NewReader("signup\nAlice\nalice@example.com\n"), os.Stdout)

	signup := botbooter.NewFlow("signup").
		Ask("name", "What's your name?").
		Ask("email", "What's your email?").
		OnComplete(func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message, a botbooter.Answers) {
			_ = b.SendMessageContext(ctx, m.ChannelID, "Welcome, "+a.Get("name")+"!")
		})
	_ = bot.HandleFlow("^signup$", signup)

	_ = bot.Run(context.Background())
	// Output:
	// What's your name?
	// What's your email?
	// Welcome, Alice!
}
