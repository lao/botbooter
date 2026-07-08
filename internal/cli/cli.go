// Package cli is the local CLI adapter for botbooter. It implements core.Adapter.
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

const (
	userID    = "cli-user"
	channelID = "cli"
)

type adapter struct {
	in  io.Reader
	out io.Writer
}

func newAdapter(in io.Reader, out io.Writer) *adapter {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	return &adapter{in: in, out: out}
}

// New creates a CLI bot backed by the given reader and writer.
func New(in io.Reader, out io.Writer) *core.Bot {
	return core.New(core.CLIBotType, newAdapter(in, out))
}

func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	// Scanning runs on its own goroutine because a blocking read on stdin cannot
	// be canceled; the lifecycle loop below selects on ctx so shutdown never
	// waits for the next line, and a line read after cancellation is dropped
	// rather than dispatched.
	lines := make(chan string)
	scanErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(a.in)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
		scanErr <- scanner.Err()
	}()

	go func() {
		for {
			select {
			case <-ctx.Done():
				deps.Done(nil)
				return
			case err := <-scanErr:
				deps.Done(err)
				return
			case line := <-lines:
				deps.Dispatch(ctx, &core.Message{
					UserID:    userID,
					ChannelID: channelID,
					Content:   line,
					Raw:       parseMessage(line),
				})
			}
		}
	}()

	return nil
}

func (a *adapter) Disconnect() error {
	return nil
}

// Send writes text to the CLI's output. The CLI has no threading, so SendOptions
// is ignored.
func (a *adapter) Send(_ context.Context, _, text string, _ core.SendOptions) error {
	_, err := fmt.Fprintln(a.out, text)
	return err
}

// Attachments returns the attachments parsed from the message's typed line.
func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	data, ok := RawData(m)
	if !ok {
		return nil, nil
	}
	return data.Attachments, nil
}

// RawData returns the parsed CLI line carried on m, reporting whether m
// originated from the CLI adapter.
func RawData(m *core.Message) (*core.CLIMessage, bool) {
	data, ok := m.Raw.(*core.CLIMessage)
	return data, ok
}

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

func looksLikePath(token string) bool {
	return strings.ContainsAny(token, `/\`) || strings.Contains(token, ".")
}

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
