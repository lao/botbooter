# LLM example

A botbooter bot that answers questions with an LLM. Any adapter (Slack, Discord,
Telegram, WhatsApp, Teams or the local CLI) is wired to
[gollm](https://github.com/teilomillet/gollm), so `ask <question>` gets a
model-generated reply threaded onto the message.

The handler is platform-agnostic — the same code runs on every adapter.

## Run

The LLM is picked from the environment:

| Var                 | Default                   | Notes                                          |
| ------------------- | ------------------------- | ---------------------------------------------- |
| `LLM_PROVIDER`      | `anthropic`               | Any provider gollm supports                    |
| `LLM_MODEL`         | small model per provider  | e.g. `gpt-4o-mini`, `claude-3-5-haiku-latest`  |
| `<PROVIDER>_API_KEY`| —                         | `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, … |

```bash
export ANTHROPIC_API_KEY=sk-ant-...
go run ./_examples/llm            # CLI mode
```

Then type:

```
ask why is the sky blue?
```

Other adapters take the same platform credentials as the `basic` example plus the
LLM vars above:

```bash
export LLM_PROVIDER=openai OPENAI_API_KEY=sk-...
export DISCORD_BOT_TOKEN=...
go run ./_examples/llm discord
```

Env vars can also live in a `.env` file next to the example.

## Test

Tests use a fake `generator`, so they run offline with no API key:

```bash
cd _examples && go test ./llm/...
```

## Layout

| File           | Purpose                                                      |
| -------------- | ----------------------------------------------------------- |
| `chat.go`      | The `generator` seam + `askHandler` — the only bot glue     |
| `llm.go`       | Builds a gollm LLM from the environment                      |
| `platforms.go` | Adapter selection (`cli`, `slack`, `discord`, …)            |
| `main.go`      | Wiring: build LLM, register handler, run                     |
| `chat_test.go` | Handler tests against a fake LLM                             |
