package botbooter_test

import (
	"context"
	"os"
	"strings"

	"github.com/lao/botbooter"
)

// Example shows the CLI adapter, which needs no external credentials: it reads
// messages from an io.Reader and writes replies to an io.Writer.
func Example() {
	bot := botbooter.InitAsCLIBot(strings.NewReader("echo hello\n"), os.Stdout)

	_ = bot.HandleFunc("^echo ", func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message) {
		_ = b.SendMessageContext(ctx, m.ChannelID, strings.TrimPrefix(m.Content, "echo "))
	})

	// Run returns when the input reaches EOF.
	_ = bot.Run(context.Background())
	// Output: hello
}
