# Platform setup

How to provision credentials for each platform botbooter supports. Slack and
Discord need an app + tokens; the CLI needs nothing.

> 📖 This page is best viewed [on GitHub](https://github.com/lao/botbooter/blob/main/docs/platforms.md) — pkg.go.dev renders the README but not this file.

**Official documentation**

- Slack — [Using Socket Mode](https://docs.slack.dev/apis/events-api/using-socket-mode/)
- Discord — [Developer Portal](https://discord.com/developers/applications) · [Gateway intents](https://discord.com/developers/docs/events/gateway)
- CLI — [`examples/v1`](../examples/v1) and the [README Quickstart](../README.md#quickstart)

---

## Slack

botbooter connects over **Socket Mode**, so you need both an *app-level
token* (`xapp-…`) and a *bot token* (`xoxb-…`).

1. **Create an app** at [api.slack.com/apps](https://api.slack.com/apps) →
   *Create New App → From scratch*. Pick a name and workspace.
2. **Enable Socket Mode** (*Settings → Socket Mode → toggle on*). When
   prompted, generate an **app-level token** with the
   [`connections:write`](https://docs.slack.dev/reference/scopes/connections.write/)
   scope — it starts with `xapp-`. (You can also create it under *Settings →
   Basic Information → App-Level Tokens*.)
3. **Add Bot Token Scopes** (*Features → OAuth & Permissions → Scopes → Bot
   Token Scopes*):
   - `chat:write` — required to send replies.
   - `channels:history` (and `im:history`, `groups:history`,
     `mpim:history` as needed) — required to **receive** message events from
     those conversation types.
4. **Enable events & subscribe** (*Features → Event Subscriptions → Enable
   Events*, then *Subscribe to bot events*): add `message.channels` (and/or
   `message.im`, …) to match the scopes above.
5. **Install to the workspace** (*OAuth & Permissions → Install to
   Workspace*) and copy the **Bot User OAuth Token** (`xoxb-…`). Re-install
   whenever you change scopes.
6. **Invite the bot** to a channel (`/invite @your-bot`) and post from a
   **human** account — the bot ignores its own and other bots' messages.

```go
bot := botbooter.InitAsSlackBot(os.Getenv("SLACK_APP_TOKEN"), os.Getenv("SLACK_BOT_TOKEN"))
```

**Environment variables** (read by the bundled example):

| Variable | Value |
|---|---|
| `SLACK_APP_TOKEN` | app-level token from step 2 (`xapp-…`) |
| `SLACK_BOT_TOKEN` | Bot User OAuth Token from step 5 (`xoxb-…`) |

#### No response?

It's almost always one of:

- **Missing bot token scopes** — no `*:history` ⇒ no events delivered even
  when subscribed; no `chat:write` ⇒ the bot can't reply.
- **The bot isn't in the channel** — run `/invite @your-bot`.
- **Re-install skipped after a scope change** — scopes only take effect once
  you re-install the app.

**Official docs:** [Using Socket Mode](https://docs.slack.dev/apis/events-api/using-socket-mode/) · [`connections:write` scope](https://docs.slack.dev/reference/scopes/connections.write/) · [Your apps dashboard](https://api.slack.com/apps)

---

## Discord

botbooter connects over the **Gateway** and requests the privileged
*Message Content Intent* so handlers can read message text.

1. **Create an application** in the
   [Discord Developer Portal](https://discord.com/developers/applications) →
   *New Application*. Open the **Bot** tab and copy the **bot token** (use
   *Reset Token* if it was never revealed).
2. **Enable the Message Content Intent** (*Bot → Privileged Gateway
   Intents → MESSAGE CONTENT INTENT*). botbooter requests this privileged
   intent so handlers receive message text — without it Discord delivers an
   empty `content`.
3. **Invite the bot** (*OAuth2 → URL Generator*): select the `bot` scope and
   the **Send Messages**, **Read Message History**, and **View Channels**
   permissions, then open the generated URL to add it to a server.

```go
bot, err := botbooter.InitAsDiscordBot(os.Getenv("DISCORD_BOT_TOKEN"))
```

**Environment variables** (read by the bundled example):

| Variable | Value |
|---|---|
| `DISCORD_BOT_TOKEN` | bot token from step 1 |

**Official docs:** [Developer Portal](https://discord.com/developers/applications) · [Gateway intents](https://discord.com/developers/docs/events/gateway) · [What are Privileged Intents?](https://support-dev.discord.com/hc/en-us/articles/6207308062871-What-are-Privileged-Intents)

---

## CLI

No credentials, no portal, no setup. The CLI adapter reads from stdin and
writes to stdout, so it's the fastest way to develop and test handlers:

```bash
go run ./examples/v1            # CLI mode (default)
```

**Environment variables:** none.

A terminal has no real upload channel, so the CLI treats any local file path
in a message as an attachment — see [Attachments](../README.md#attachments)
in the README. **Use it with trusted local input only.**

**Official docs:** the bundled [`examples/v1`](../examples/v1) and the
[README Quickstart](../README.md#quickstart).

---

[← Back to the README](../README.md)
