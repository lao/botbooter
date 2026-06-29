// Package cli exposes the local-CLI constructor and raw-message accessor for
// botbooter. Import it when your bot runs on the local CLI; it pulls in no
// platform SDK, so a CLI-only binary never compiles discordgo, slack-go or
// go-telegram.
package cli

import (
	"io"

	"github.com/lao/botbooter"
	cliint "github.com/lao/botbooter/internal/cli"
)

// New creates a CLI bot backed by the given reader and writer.
func New(in io.Reader, out io.Writer) *botbooter.Bot {
	return cliint.New(in, out)
}

// RawData returns the parsed CLI line carried on m, reporting whether m
// originated from the CLI adapter.
func RawData(m *botbooter.Message) (*botbooter.CLIMessage, bool) {
	return cliint.RawData(m)
}
