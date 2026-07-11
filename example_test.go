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
