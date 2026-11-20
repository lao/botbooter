package main

import (
	"strings"
	"testing"

	"github.com/lao/botbooter/_examples/internal/exampletest"
)

func TestLLMCLIUsageHint(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("OPENAI_API_KEY", "sk-"+strings.Repeat("x", 25))
	got := exampletest.RunCLI(t, main, "hello\n")
	if !strings.Contains(got, "Say `ask <question>`") {
		t.Fatalf("stdout = %q", got)
	}
}

func TestPlatformConstructors(t *testing.T) {
	exampletest.CheckPlatforms(t, newBot, []string{"cli", "slack", "discord", "telegram", "whatsapp", "teams", "github", "gitlab", "signal"})
}
