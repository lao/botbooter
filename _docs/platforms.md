# Platform setup

How to provision credentials for each platform botbooter supports. Slack,
Discord and Telegram need tokens (from their app portals or BotFather); WhatsApp
and Microsoft Teams need cloud credentials plus a public HTTPS webhook; the CLI
needs nothing.

> 📖 This page is best viewed [on GitHub](https://github.com/lao/botbooter/blob/main/_docs/platforms.md), pkg.go.dev renders the README but not this file.

**Official documentation**

- Slack, [Using Socket Mode](https://docs.slack.dev/apis/events-api/using-socket-mode/)
- Discord, [Developer Portal](https://discord.com/developers/applications) · [Gateway intents](https://discord.com/developers/docs/events/gateway)
- Telegram, [BotFather](https://t.me/BotFather) · [Bot API](https://core.telegram.org/bots/api)
- WhatsApp, [Cloud API](https://developers.facebook.com/docs/whatsapp/cloud-api) · [Webhooks getting started](https://developers.facebook.com/docs/graph-api/webhooks/getting-started)
- Microsoft Teams, [Azure Bot resource](https://learn.microsoft.com/azure/bot-service/abs-quickstart) · [Bot Framework authentication](https://learn.microsoft.com/azure/bot-service/rest-api/bot-framework-rest-connector-authentication)
- CLI, [`_examples/v1`](../_examples/v1) and the [README Quickstart](../README.md#quickstart)

---

## Slack

botbooter connects over **Socket Mode**, so you need both an *app-level
token* (`xapp-…`) and a *bot token* (`xoxb-…`).

1. **Create an app** at [api.slack.com/apps](https://api.slack.com/apps) →
   *Create New App → From scratch*. Pick a name and workspace.
2. **Enable Socket Mode** (*Settings → Socket Mode → toggle on*). When
   prompted, generate an **app-level token** with the
   [`connections:write`](https://docs.slack.dev/reference/scopes/connections.write/)
   scope, it starts with `xapp-`. (You can also create it under *Settings →
   Basic Information → App-Level Tokens*.)
3. **Add Bot Token Scopes** (*Features → OAuth & Permissions → Scopes → Bot
   Token Scopes*):
   - `chat:write`, required to send replies.
   - `channels:history` (and `im:history`, `groups:history`,
     `mpim:history` as needed), required to **receive** message events from
     those conversation types.
4. **Enable events & subscribe** (*Features → Event Subscriptions → Enable
   Events*, then *Subscribe to bot events*): add `message.channels` (and/or
   `message.im`, …) to match the scopes above.
5. **Install to the workspace** (*OAuth & Permissions → Install to
   Workspace*) and copy the **Bot User OAuth Token** (`xoxb-…`). Re-install
   whenever you change scopes.
6. **Invite the bot** to a channel (`/invite @your-bot`) and post from a
   **human** account, the bot ignores its own and other bots' messages.

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

- **Missing bot token scopes**: no `*:history` ⇒ no events delivered even
  when subscribed; no `chat:write` ⇒ the bot can't reply.
- **The bot isn't in the channel**: run `/invite @your-bot`.
- **Re-install skipped after a scope change**: scopes only take effect once
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
   intent so handlers receive message text, without it Discord delivers an
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

botbooter connects over the Bot API **getUpdates long-poll loop**, outbound
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

An empty token is rejected at construction; an otherwise-invalid token is not,
it surfaces later as the long-poll loop's own authentication errors (the loop
logs and retries rather than failing fast).

**Environment variables** (read by the bundled example):

| Variable | Value |
|---|---|
| `TELEGRAM_BOT_TOKEN` | bot token from step 1 (`123456:ABC-…`) |

**Resolving attachment URLs.** Telegram delivers media by file id, not URL, so
`Attachment.URL` is empty. Call `bot.ResolveAttachmentURL(ctx, att)` to fetch a
download link via the Bot API `getFile`. That link **embeds the bot token in
plaintext**, treat it as a secret and never log it. Each successful resolve
logs a one-line warning to that effect; set the
`BOTBOOTER_TELEGRAM_SUPPRESS_URL_WARNING` environment variable to any non-empty
value to silence it.

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
   - **Access token** (`WA_TOKEN`): prefer a long-lived **system-user** token;
     the default test token expires in ~24h, after which `Send` fails.
   - **Phone number ID** (`WA_PHONE_ID`): the sender id on the API Setup page
     (not the display phone number).
   - **App secret** (`WA_APP_SECRET`): *App Settings → Basic → App Secret*. It
     verifies the `X-Hub-Signature-256` HMAC on every inbound request; without it
     the endpoint would accept spoofed payloads.
3. **Choose a verify token** (`WA_VERIFY_TOKEN`): any string you pick. Meta
   echoes it back during the one-time webhook handshake.
4. **Expose the webhook**: run the bot bound to `WA_ADDR` (e.g. `:8080`), put
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

- **403 on every callback**: the `X-Hub-Signature-256` HMAC failed:
  `WA_APP_SECRET` doesn't match the app, or a proxy mutates the request body
  before it reaches the bot (the signature is computed over the raw bytes).
- **Webhook won't verify**: the handshake `GET` needs `WA_VERIFY_TOKEN` to match
  exactly and your public HTTPS URL to reach `Addr`.
- **`Send` fails**: an expired token, or you're outside the 24-hour window
  (a template message is required there).

**Official docs:** [Cloud API](https://developers.facebook.com/docs/whatsapp/cloud-api) · [Webhooks](https://developers.facebook.com/docs/graph-api/webhooks/getting-started) · [Webhook components](https://developers.facebook.com/docs/whatsapp/cloud-api/webhooks/components)

---

## Microsoft Teams

botbooter speaks the **Azure Bot Framework** (Bot Connector REST API). Like
WhatsApp, the channel delivers inbound messages as **HTTP webhook callbacks**, so
the adapter runs its own server: it binds a local `Addr`, and you put a
**TLS-terminating reverse proxy** in front and register the public HTTPS URL as
the Azure Bot's messaging endpoint. Replies go back over the Bot Connector API.

The quickest path is to register the bot, run botbooter locally, tunnel a public
HTTPS URL to it with [ngrok](https://ngrok.com/download), and only then point the
bot's messaging endpoint at that URL, so you can iterate without deploying.

#### Step 1, Register the bot

Create an **Azure Bot resource** in the
[Azure portal](https://learn.microsoft.com/azure/bot-service/abs-quickstart)
(*Create a resource → Azure Bot*), or register a web service directly on the
[Bot Framework](https://dev.botframework.com/bots) dashboard, see
[Create a bot for Teams → Register your web service](https://learn.microsoft.com/microsoftteams/platform/bots/how-to/create-a-bot-for-teams#register-your-web-service-with-the-bot-framework).
Choose **multi-tenant** or **single-tenant**; the type determines whether you set
`TEAMS_APP_TENANT_ID` below. Then collect the credentials
(*Azure Bot → Configuration / Microsoft App ID*):

- **Microsoft App ID** (`TEAMS_APP_ID`): the bot's app (client) id. It is the
  expected audience of inbound tokens and the client id used to mint outbound
  tokens.
- **Client secret** (`TEAMS_APP_PASSWORD`): create one under the app's
  *Certificates & secrets*; it pairs with the App ID.
- **Tenant ID** (`TEAMS_APP_TENANT_ID`): **only for single-tenant** bots. Leave
  it unset for multi-tenant bots (the multi-tenant token endpoint is used).

**Do not set a messaging endpoint yet**, you'll get the public URL from ngrok in
step 3.

#### Step 2, Run the bot locally

Export the credentials and run the bundled example. Bind `TEAMS_ADDR` to a local
port (e.g. `:3978`, the Bot Framework convention):

```bash
export TEAMS_APP_ID=MICROSOFT_APP_ID
export TEAMS_APP_PASSWORD=MICROSOFT_APP_PASSWORD
export TEAMS_ADDR=:3978

go run ./_examples/v1 teams
```

This starts the webhook server listening on port `3978` (path `/api/messages`).

#### Step 3, Expose the local server with ngrok

In a separate terminal, tunnel a public HTTPS URL to your local port:

```bash
ngrok http 3978
```

Copy the `https://…` forwarding URL. This is your public endpoint; botbooter
still binds plain HTTP locally, and ngrok (or your own reverse proxy in
production) terminates TLS in front of it.

#### Step 4, Point the bot at your endpoint

Set the bot's **messaging endpoint** to `https://…/api/messages` (your ngrok URL
plus the path) in the Azure Bot resource or the
[Bot Framework](https://dev.botframework.com/bots) dashboard, then add the Teams
channel (*Azure Bot → Channels → Microsoft Teams*).

#### Step 5, Test the bot

Test from the Bot Framework portal's **Test in Web Chat**, or create an app
manifest and side-load the app into Teams, see
[Create your app manifest and package](https://learn.microsoft.com/microsoftteams/platform/bots/how-to/create-a-bot-for-teams#create-your-app-manifest-and-package).
Post from a **human** account; the bot ignores its own and other bots' messages.

```go
import "github.com/lao/botbooter/teams"

bot, err := teams.New(teams.Config{
	AppID:       os.Getenv("TEAMS_APP_ID"),
	AppPassword: os.Getenv("TEAMS_APP_PASSWORD"),
	TenantID:    os.Getenv("TEAMS_APP_TENANT_ID"), // optional; single-tenant only
	Addr:        os.Getenv("TEAMS_ADDR"),          // e.g. ":3978"; a bare "3978" is accepted
})
```

Optional `teams.Config` fields: `Path` (webhook route, default `/api/messages`)
and `HTTPClient` (the outbound HTTP client; defaults to a 30-second timeout).

Every inbound request is authenticated by validating the **Bot Connector JWT**
(JWKS signature, audience == App ID, issuer, and a `serviceurl` claim that must
match the Activity), and outbound replies only go to allowlisted Bot Framework
hosts. The JWT authenticates the connector but the Activity body is
channel-trusted (not individually signed) and there is no replay tracking, so
**terminate TLS at a trusted proxy and never expose `Addr` in cleartext**; do
rate limiting there too. The **Bot Framework Emulator** uses an AAD issuer and is
not supported.

**Environment variables** (read by the bundled example):

| Variable | Value |
|---|---|
| `TEAMS_APP_ID` | Microsoft App ID from step 1 |
| `TEAMS_APP_PASSWORD` | client secret from step 1 |
| `TEAMS_APP_TENANT_ID` | optional; tenant id for a **single-tenant** bot |
| `TEAMS_ADDR` | local bind address, e.g. `:3978` (a bare port is accepted) |
| `TEAMS_PATH` | optional webhook route; defaults to `/api/messages` |

#### No messages?

- **401 on every callback**, JWT validation failed: a wrong `TEAMS_APP_ID`
  (audience mismatch), a clock skew beyond 5 minutes, or the request came from the
  unsupported Emulator. The rejection reason is logged.
- **Replies fail / `Send` errors**, a wrong `TEAMS_APP_PASSWORD` (token mint
  fails), a `TEAMS_APP_TENANT_ID` set for a multi-tenant bot (or missing for a
  single-tenant one), or no inbound Activity has been seen yet for that
  conversation (the serviceUrl is learned from inbound messages).
- **Endpoint never hit**, the Azure Bot messaging endpoint must be your public
  `https://…/api/messages` and reach `Addr` through the proxy.

**Official docs:** [Create a bot for Teams](https://learn.microsoft.com/microsoftteams/platform/bots/how-to/create-a-bot-for-teams) · [Azure Bot quickstart](https://learn.microsoft.com/azure/bot-service/abs-quickstart) · [Bot Framework dashboard](https://dev.botframework.com/bots) · [Connector authentication](https://learn.microsoft.com/azure/bot-service/rest-api/bot-framework-rest-connector-authentication) · [Send & receive messages](https://learn.microsoft.com/azure/bot-service/rest-api/bot-framework-rest-connector-create-messages) · [ngrok](https://ngrok.com/download)

---

## CLI

No credentials, no portal, no setup. The CLI adapter reads from stdin and
writes to stdout, so it's the fastest way to develop and test handlers:

```bash
go run ./_examples/v1            # CLI mode (default)
```

**Environment variables:** none.

A terminal has no real upload channel, so the CLI treats any local file path
in a message as an attachment, see [Attachments](../README.md#attachments)
in the README. **Use it with trusted local input only.**

**Official docs:** the bundled [`_examples/v1`](../_examples/v1) and the
[README Quickstart](../README.md#quickstart).

---

## Threaded replies

Threading is a **send option**, not a separate method. Pass one to
`b.SendMessageContext` (or `b.SendMessage`); with no option a send is a plain
channel message. `b.Reply(ctx, m, text)` is sugar for the common case —
`SendMessageContext(ctx, m.ChannelID, text, InReplyTo(m))`.

Two options carry the anchor:

- **`botbooter.InReplyTo(m)`** — pass the whole inbound `Message`; each adapter
  derives its own correct anchor from it. You never compute a thread id.
- **`botbooter.WithThreadID(id)`** — a raw native anchor the adapter uses
  verbatim. It **takes precedence** over `InReplyTo`. Its meaning is
  platform-specific (see the table), so you own platform-correctness.

`InReplyTo` derives from one of two fields off the `Message`:

- **`m.ReplyToID`** — the thread root / replied-to id the platform delivered
  (`""` when the message is top-level in a channel).
- **`m.ID`** — the message's own id.

### Per-platform anchor

| Platform | `InReplyTo(m)` derives from | Raw `WithThreadID(id)` is | On the wire | When the anchor is empty |
|---|---|---|---|---|
| **Slack** | `m.ReplyToID` (the thread root, `ThreadTimeStamp`) | a `thread_ts` | `thread_ts` on `chat.postMessage` | top-level message ⇒ plain top-level reply (does **not** open a new thread off `m.ID`; a raw `WithThreadID` of a top-level ts *will* start one) |
| **Discord** | `m.ID` | a **reply message id** (not a Discord thread-channel id) | inline reply via `message_reference.message_id` | plain channel send |
| **Telegram** | `m.ID` | a **reply message id** | `reply_parameters.message_id` | plain send. A *derived* id that isn't a positive integer degrades to a plain send; an *explicit* `WithThreadID` that isn't returns an error |
| **WhatsApp** | `m.ID` | a **quote message id** | `context.message_id` (a quoted reply) | plain send (no `context`) |
| **Teams** | — (options ignored) | — | — | always a plain channel send |
| **CLI** | — (options ignored) | — | — | always a plain channel send |

The reply anchor assumes you send to the message's **own channel**; e.g. a
cross-channel Discord `InReplyTo` builds a `message_reference` for another
channel, which Discord rejects.

### Why the anchor differs

`ReplyToID` means different things per platform: on Slack it's the **thread
root**, so replying with it keeps the answer in the same thread; on
Discord/Telegram it's the specific **replied-to message**, and the natural reply
target is the *received* message itself (`m.ID`), not the chain root. A single
core-computed anchor would mis-anchor half the platforms, so each adapter owns
the choice — which is why `InReplyTo` carries the whole `Message` rather than a
pre-computed id.

### The two scenarios

- **The triggering message is already inside a thread.** `InReplyTo(m)` continues
  that same thread — Slack anchors on the thread root (`m.ReplyToID`);
  Discord/Telegram/WhatsApp reference/quote `m.ID`.
- **The triggering message is a top-level channel comment.** `InReplyTo(m)`
  answers that comment directly, anchored on it (Discord inline reply, Telegram
  `reply_to_message_id`, WhatsApp quote). On Slack a top-level message has no
  thread root, so the reply is a plain top-level message in the channel — the
  `InReplyTo` path intentionally does **not** start a fresh thread off it (use a
  raw `WithThreadID(m.ID)` if you deliberately want to open one).

### Fallback & errors

A send degrades to a plain channel message when the adapter ignores the options
(Teams, CLI) **or** the anchor resolves to nothing — it never fails just because
a message can't be threaded. The one loud exception is an explicit
`WithThreadID` that a platform can't use (an id that isn't a positive message id
on Telegram), which returns an error rather than silently dropping. `Reply` returns an error only
when the bot has no adapter or `m` is `nil`.

To reach beyond these normalized semantics (Slack broadcast replies, message
edits, etc.), drop to the raw SDK client via the per-platform accessors — see
[Raw platform access](../README.md#raw-platform-access).

---

[← Back to the README](../README.md)
