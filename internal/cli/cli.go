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

func New(in io.Reader, out io.Writer) *core.Bot {
	return core.New(core.CLIBotType, newAdapter(in, out))
}

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
		deps.Done(scanner.Err())
	}()

	return nil
}

func (a *adapter) Disconnect() error {
	return nil
}

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
