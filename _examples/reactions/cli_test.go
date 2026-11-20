package main

import (
	"strings"
	"testing"

	"github.com/lao/botbooter/_examples/internal/exampletest"
)

func TestReactionsCLI(t *testing.T) {
	got := exampletest.RunCLI(t, main, "echo hello\n")
	if !strings.Contains(got, "You said: hello — now add an emoji reaction") {
		t.Fatalf("stdout = %q", got)
	}
}

func TestPlatformConstructors(t *testing.T) {
	exampletest.CheckPlatforms(t, newBot, []string{"cli", "slack", "discord", "telegram", "whatsapp", "teams", "github", "gitlab", "signal"})
}
