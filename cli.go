package botbooter

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
)

// CLI identifiers reported on messages read from the CLI adapter.
const (
	cliUserID    = "cli-user"
	cliChannelID = "cli"
)

// InitAsCLIBot creates a bot that reads newline-delimited messages from in and
// writes replies to out. It requires no external credentials, which makes it
// ideal for local development and tests. When in or out is nil, os.Stdin and
// os.Stdout are used respectively.
//
// The adapter is intended for trusted, local input only: whitespace-separated
// tokens in a message that resolve to existing local files are opened and
// attached (see parseCLIMessage), so it must not be wired to untrusted input.
//
// Because the package does not own in, the background reader blocks inside a
// read until the next line or EOF. If in never reaches EOF (e.g. os.Stdin),
// that goroutine stays blocked after Disconnect/context cancellation until more
// input arrives, so repeated Connect/Disconnect cycles can leak one goroutine
// per cycle.
func InitAsCLIBot(in io.Reader, out io.Writer) *Bot {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	return &Bot{
		BotType: CLIBotType,
		cliIn:   in,
		cliOut:  out,
	}
}

// connectCLI starts reading lines from the input in the background. Each line
// is dispatched as a message. When the input reaches EOF the run loop is
// signaled to stop.
func (b *Bot) connectCLI(ctx context.Context) error {
	scanner := bufio.NewScanner(b.cliIn)
	done := b.done

	go func() {
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}

			line := scanner.Text()
			b.dispatch(ctx, &Message{
				UserID:    cliUserID,
				ChannelID: cliChannelID,
				Content:   line,
				CLIData:   parseCLIMessage(line),
			})
		}
		// Input exhausted: tell Run to shut down. done is buffered, so this
		// never blocks even if no one is reading.
		done <- scanner.Err()
	}()

	return nil
}

func (b *Bot) disconnectCLI() error {
	return nil
}

// parseCLIMessage builds a CLIMessage from a typed line. Since a terminal has
// no real upload channel, any whitespace-separated token that resolves to an
// existing local file is treated as an attachment ("uploading" a file means
// referencing its path in the message).
func parseCLIMessage(line string) *CLIMessage {
	msg := &CLIMessage{Text: line}
	for _, token := range strings.Fields(line) {
		if !looksLikePath(token) {
			continue
		}
		if att, ok := cliFileAttachment(token); ok {
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

// cliFileAttachment turns a path into an Attachment if it points to an existing
// regular file, sniffing the content type to set IsImage.
func cliFileAttachment(path string) (Attachment, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return Attachment{}, false
	}

	att := Attachment{URL: path, ExtraData: path}
	if f, err := os.Open(path); err == nil {
		defer func() { _ = f.Close() }()
		buf := make([]byte, 512)
		n, _ := f.Read(buf)
		att.IsImage = strings.HasPrefix(http.DetectContentType(buf[:n]), "image/")
	}
	return att, true
}
