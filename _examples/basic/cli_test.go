package main

import (
	"testing"

	"github.com/lao/botbooter/_examples/internal/exampletest"
)

func TestBasicCLI(t *testing.T) {
	got := exampletest.RunCLI(t, main, "unknown\necho hello\n")
	if got != "You said: hello\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestPlatformConstructors(t *testing.T) {
	exampletest.CheckPlatforms(t, newBot, []string{"cli", "slack", "discord", "telegram", "whatsapp", "teams", "github", "gitlab", "signal", "bitbucket"})
}
