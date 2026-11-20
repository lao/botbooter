package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/lao/botbooter/cli"

	"github.com/lao/botbooter/_examples/internal/exampletest"
)

func TestMultiCLIFallback(t *testing.T) {
	for _, key := range []string{"SLACK_APP_TOKEN", "DISCORD_BOT_TOKEN", "TELEGRAM_BOT_TOKEN", "WA_TOKEN", "WA_MEOW_DB", "TEAMS_APP_ID"} {
		t.Setenv(key, "")
	}
	got := exampletest.RunCLI(t, main, "echo hello\n")
	if got != "[cli] You said: hello\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestRunAllStopsPeersOnError(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	peer := namedBot{"peer", cli.New(reader, io.Discard)}
	failure := errors.New("input closed")
	brokenReader, brokenWriter := io.Pipe()
	defer brokenReader.Close()
	if err := brokenWriter.CloseWithError(failure); err != nil {
		t.Fatal(err)
	}
	broken := namedBot{"broken", cli.New(brokenReader, io.Discard)}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := runAll(ctx, []namedBot{peer, broken}); !errors.Is(err, failure) {
		t.Fatalf("runAll = %v, want input error", err)
	}
	if ctx.Err() != nil {
		t.Fatal("peer remained active until external timeout")
	}
}
