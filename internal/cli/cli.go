// Package cli is the local CLI adapter for botbooter. It reads newline-delimited
// messages from an io.Reader and writes replies to an io.Writer, requiring no
// external credentials. It implements core.Adapter.
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/lao/botbooter/internal/core"
)

// CLI identifiers reported on messages read from the CLI adapter.
const (
	userID    = "cli-user"
	channelID = "cli"
)

// adapter is the CLI implementation of core.Adapter.
type adapter struct {
	in  io.Reader
	out io.Writer
}

// newAdapter builds a CLI adapter, defaulting in/out to os.Stdin/os.Stdout when
// nil.
func newAdapter(in io.Reader, out io.Writer) *adapter {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	return &adapter{in: in, out: out}
}

// New creates a bot that reads newline-delimited messages from in and writes
// replies to out. It requires no external credentials, which makes it ideal for
// local development and tests. When in or out is nil, os.Stdin and os.Stdout
// are used respectively.
//
// The adapter is intended for trusted, local input only: whitespace-separated
// tokens in a message that resolve to existing local files are opened and
// attached (see parseMessage), so it must not be wired to untrusted input.
//
// Because the package does not own in, the background reader blocks inside a
// read until the next line or EOF. If in never reaches EOF (e.g. os.Stdin),
// that goroutine stays blocked after Disconnect/context cancellation until more
// input arrives, so repeated Connect/Disconnect cycles can leak one goroutine
// per cycle.
func New(in io.Reader, out io.Writer) *core.Bot {
	return core.New(core.CLIBotType, newAdapter(in, out))
}

// Connect starts reading lines from the input in the background. Each line is
// dispatched as a message. When the input reaches EOF the run loop is signaled
// to stop.
func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	scanner := bufio.NewScanner(a.in)

	go func() {
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}

			line := scanner.Text()
			deps.Dispatch(ctx, &core.Message{
				UserID:    userID,
				ChannelID: channelID,
				Content:   line,
				CLIData:   parseMessage(line),
			})
		}
		// Input exhausted: tell Run to shut down. done is buffered, so this
		// never blocks even if no one is reading.
		deps.Done(scanner.Err())
	}()

	return nil
}

// Disconnect is a no-op: the reader loop stops on context cancellation, so there
// is nothing to tear down.
func (a *adapter) Disconnect() error {
	return nil
}

// Send writes text as a line to the adapter's output. The channel and context
// are ignored; the CLI has a single output stream.
func (a *adapter) Send(_ context.Context, _, text string) error {
	_, err := fmt.Fprintln(a.out, text)
	return err
}

// Attachments returns the attachments parsed from the message's typed line.
func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	if m.CLIData == nil {
		return nil, nil
	}
	return m.CLIData.Attachments, nil
}

// parseMessage builds a CLIMessage from a typed line. Since a terminal has no
// real upload channel, any whitespace-separated token that resolves to an
// existing local file is treated as an attachment ("uploading" a file means
// referencing its path in the message).
func parseMessage(line string) *core.CLIMessage {
	msg := &core.CLIMessage{Text: line}
	for _, token := range strings.Fields(line) {
		if !looksLikePath(token) {
			continue
		}
		if att, ok := fileAttachment(token); ok {
			msg.Attachments = append(msg.Attachments, att)
		}
	}
	return msg
}

// looksLikePath cheaply filters tokens worth stat-ing: those containing a path
// separator or a dotted extension. It keeps plain words from being stat-ed.
func looksLikePath(token string) bool {
	return strings.ContainsAny(token, `/\`) || strings.Contains(token, ".")
}

// fileAttachment turns a path into an Attachment if it points to an existing
// regular file, sniffing the content type to set IsImage.
func fileAttachment(path string) (core.Attachment, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return core.Attachment{}, false
	}

	att := core.Attachment{URL: path, ExtraData: path}
	if f, err := os.Open(path); err == nil {
		defer func() { _ = f.Close() }()
		buf := make([]byte, 512)
		n, _ := f.Read(buf)
		att.IsImage = strings.HasPrefix(http.DetectContentType(buf[:n]), "image/")
	}
	return att, true
}
