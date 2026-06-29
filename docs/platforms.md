# Platform setup

How to provision credentials for each platform botbooter supports. Slack,
Discord and Telegram need tokens (from their app portals or BotFather); WhatsApp
needs Cloud API credentials plus a public HTTPS webhook; the CLI needs nothing.

> 📖 This page is best viewed [on GitHub](https://github.com/lao/botbooter/blob/main/docs/platforms.md) — pkg.go.dev renders the README but not this file.

**Official documentation**

- Slack — [Using Socket Mode](https://docs.slack.dev/apis/events-api/using-socket-mode/)
- Discord — [Developer Portal](https://discord.com/developers/applications) · [Gateway intents](https://discord.com/developers/docs/events/gateway)
- Telegram — [BotFather](https://t.me/BotFather) · [Bot API](https://core.telegram.org/bots/api)
- WhatsApp — [Cloud API](https://developers.facebook.com/docs/whatsapp/cloud-api) · [Webhooks getting started](https://developers.facebook.com/docs/graph-api/webhooks/getting-started)
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
import "github.com/lao/botbooter/slack"

bot := slack.New(os.Getenv("SLACK_APP_TOKEN"), os.Getenv("SLACK_BOT_TOKEN"))
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
import "github.com/lao/botbooter/discord"

bot, err := discord.New(os.Getenv("DISCORD_BOT_TOKEN"))
```

**Environment variables** (read by the bundled example):

| Variable | Value |
|---|---|
| `DISCORD_BOT_TOKEN` | bot token from step 1 |

**Official docs:** [Developer Portal](https://discord.com/developers/applications) · [Gateway intents](https://discord.com/developers/docs/events/gateway) · [What are Privileged Intents?](https://support-dev.discord.com/hc/en-us/articles/6207308062871-What-are-Privileged-Intents)

---

## Telegram

botbooter connects over the Bot API **getUpdates long-poll loop** — outbound
HTTPS only, so there's no public endpoint, port, or webhook to host (the
dial-out model of Slack Socket Mode and the Discord Gateway).

1. **Create a bot** by messaging [@BotFather](https://t.me/BotFather) and
   sending `/newbot`. Follow the prompts (a display name, then a username
   ending in `bot`) and copy the **bot token** it returns (`123456:ABC-…`).
2. **(Optional) Disable privacy mode** (*BotFather → `/setprivacy` → pick the
   bot → Disable*) so the bot receives **all** group messages, not only those
   that start with `/` or @-mention it. Private chats always deliver every
   message; this setting only affects groups.
3. **Message the bot** — open a private chat (or add it to a group) and post
   from a **human** account. The bot ignores its own and other bots' messages.

```go
import "github.com/lao/botbooter/telegram"

bot, err := telegram.New(os.Getenv("TELEGRAM_BOT_TOKEN"))
```

An empty token is rejected at construction; an otherwise-invalid token is not —
it surfaces later as the long-poll loop's own authentication errors (the loop
logs and retries rather than failing fast).

**Environment variables** (read by the bundled example):

| Variable | Value |
|---|---|
| `TELEGRAM_BOT_TOKEN` | bot token from step 1 (`123456:ABC-…`) |

**Official docs:** [BotFather](https://t.me/BotFather) · [Bot API](https://core.telegram.org/bots/api) · [getUpdates / long polling](https://core.telegram.org/bots/api#getupdates)

---

## WhatsApp

botbooter speaks the **Meta WhatsApp Business Cloud API**. Unlike the dial-out
platforms, the Cloud API delivers inbound messages as **HTTP webhook callbacks**,
so the adapter runs its own server: it binds a local `Addr`, and you put a
**TLS-terminating reverse proxy** in front and register the public HTTPS URL with
Meta. Outbound replies go back over the Cloud API.

1. **Create a Meta app** at
   [developers.facebook.com/apps](https://developers.facebook.com/apps) →
   *Create App*, then add the **WhatsApp** product. The dashboard provides a test
   phone number and a temporary token to get started.
2. **Collect the credentials** (*WhatsApp → API Setup*):
   - **Access token** (`WA_TOKEN`) — prefer a long-lived **system-user** token;
     the default test token expires in ~24h, after which `Send` fails.
   - **Phone number ID** (`WA_PHONE_ID`) — the sender id on the API Setup page
     (not the display phone number).
   - **App secret** (`WA_APP_SECRET`) — *App Settings → Basic → App Secret*. It
     verifies the `X-Hub-Signature-256` HMAC on every inbound request; without it
     the endpoint would accept spoofed payloads.
3. **Choose a verify token** (`WA_VERIFY_TOKEN`) — any string you pick. Meta
   echoes it back during the one-time webhook handshake.
4. **Expose the webhook** — run the bot bound to `WA_ADDR` (e.g. `:8080`), put
   HTTPS in front (a reverse proxy, or a tunnel like ngrok for local testing),
   then in *WhatsApp → Configuration → Webhook* set the **callback URL** to your
   public `https://…/webhook` and the **verify token** to the same
   `WA_VERIFY_TOKEN`, and subscribe to the **messages** field.

```go
import "github.com/lao/botbooter/whatsapp"

bot, err := whatsapp.New(whatsapp.Config{
	Token:         os.Getenv("WA_TOKEN"),
	PhoneNumberID: os.Getenv("WA_PHONE_ID"),
	AppSecret:     os.Getenv("WA_APP_SECRET"),
	VerifyToken:   os.Getenv("WA_VERIFY_TOKEN"),
	Addr:          os.Getenv("WA_ADDR"), // e.g. ":8080"; a bare "8080" is accepted
})
```

Optional `whatsapp.Config` fields: `Path` (webhook route, default `/webhook`),
`GraphVersion` (Graph API version, default `v23.0`), and `HTTPClient` (the
outbound HTTP client; defaults to a 30-second timeout).

Inbound media arrives **by id, not URL**: `Attachment.ExtraData` holds a
`*whatsapp.Media`; resolve the bytes with `GET /{media-id}` using your access
token. `Send` delivers free-form text only inside the 24-hour customer-service
window; outside it, Meta requires a pre-approved template (not yet supported).

**Environment variables** (read by the bundled example):

| Variable | Value |
|---|---|
| `WA_TOKEN` | Cloud API access token from step 2 |
| `WA_PHONE_ID` | phone number id from step 2 |
| `WA_APP_SECRET` | app secret from step 2 (HMAC verification) |
| `WA_VERIFY_TOKEN` | the verify token you chose in step 3 |
| `WA_ADDR` | local bind address, e.g. `:8080` (a bare port is accepted) |
| `WA_PATH` | optional webhook route; defaults to `/webhook` |

#### No messages?

- **403 on every callback** — the `X-Hub-Signature-256` HMAC failed:
  `WA_APP_SECRET` doesn't match the app, or a proxy mutates the request body
  before it reaches the bot (the signature is computed over the raw bytes).
- **Webhook won't verify** — the handshake `GET` needs `WA_VERIFY_TOKEN` to match
  exactly and your public HTTPS URL to reach `Addr`.
- **`Send` fails** — an expired token, or you're outside the 24-hour window
  (a template message is required there).

**Official docs:** [Cloud API](https://developers.facebook.com/docs/whatsapp/cloud-api) · [Webhooks](https://developers.facebook.com/docs/graph-api/webhooks/getting-started) · [Webhook components](https://developers.facebook.com/docs/whatsapp/cloud-api/webhooks/components)

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
