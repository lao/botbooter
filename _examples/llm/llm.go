package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/teilomillet/gollm"
)

// defaultModels maps a provider to a sensible small/fast default so the example
// runs with only an API key set. Override any of them with LLM_MODEL.
var defaultModels = map[string]string{
	"anthropic": "claude-3-5-haiku-latest",
	"openai":    "gpt-4o-mini",
	"gemini":    "gemini-1.5-flash",
}

// newLLM builds an LLM from the environment:
//
//	LLM_PROVIDER   provider name gollm understands (default "anthropic")
//	LLM_MODEL      model id (default: a small model for the provider)
//	<PROVIDER>_API_KEY   e.g. ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY
//
// The provider string is passed straight through to gollm, so any provider gollm
// supports works — this example is deliberately not hard-wired to one vendor.
func newLLM() (generator, error) {
	provider := envOr("LLM_PROVIDER", "anthropic")
	model := envOr("LLM_MODEL", defaultModels[provider])
	if model == "" {
		return nil, fmt.Errorf("no default model for provider %q; set LLM_MODEL", provider)
	}

	keyEnv := strings.ToUpper(provider) + "_API_KEY"
	apiKey := os.Getenv(keyEnv)
	if apiKey == "" {
		return nil, fmt.Errorf("%s is not set (provider %q)", keyEnv, provider)
	}

	return gollm.NewLLM(
		gollm.SetProvider(provider),
		gollm.SetModel(model),
		gollm.SetAPIKey(apiKey),
		gollm.SetMaxTokens(500),
	)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
