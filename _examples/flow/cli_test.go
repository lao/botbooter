package main

import (
	"strings"
	"testing"

	"github.com/lao/botbooter/_examples/internal/exampletest"
)

func TestSignupCLI(t *testing.T) {
	got := exampletest.RunCLI(t, main, "signup\nAda\ninvalid\ntest@example.org\nsecret\nsignup\ncancel\n")
	remaining := got
	for _, want := range []string{"What's your name?", "What's your email?", "that doesn't look like an email", "Choose a password.", "You're all set", "signup cancelled"} {
		_, rest, found := strings.Cut(remaining, want)
		if !found {
			t.Fatalf("missing %q in stdout %q", want, got)
		}
		remaining = rest
	}
	if strings.Contains(got, "secret") {
		t.Fatalf("password echoed to stdout: %q", got)
	}
}
