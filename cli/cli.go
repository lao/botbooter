// Package cli exposes the local-CLI constructor and raw-message accessor for
// botbooter. Import it when your bot runs on the local CLI; it pulls in no
// platform SDK, so a CLI-only binary never compiles discordgo, slack-go or
// go-telegram.
//
// Security: this adapter is for trusted local input only. Any path-like token
// (one containing a slash or a dot) that names an existing local file is opened
// as an attachment, so untrusted input can make the process read arbitrary files
// the operator can access. Wire it only to a trusted local source (an operator's
// terminal); never feed it network or otherwise untrusted data.
package cli

import (
	"io"

	"github.com/lao/botbooter"
	cliint "github.com/lao/botbooter/internal/cli"
)

// Message is the raw payload of a CLI message: the typed line plus any
// tokens that resolved to local-file attachments.
type Message = cliint.Message

// New creates a CLI bot backed by the given reader and writer. A nil in or out
// defaults to os.Stdin or os.Stdout respectively.
//
// The reader must carry trusted local input only: any path-like token (one
// containing a slash or a dot) that names an existing local file is opened as an
// attachment, so untrusted input can make the process read arbitrary accessible
// files. Never back it with a network or otherwise untrusted source.
func New(in io.Reader, out io.Writer) *botbooter.Bot {
	return cliint.New(in, out)
}

// RawData returns the parsed CLI line carried on m, reporting whether m
// originated from the CLI adapter.
func RawData(m *botbooter.Message) (*Message, bool) {
	return cliint.RawData(m)
}
