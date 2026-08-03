# Platform setup

How to provision credentials for each platform botbooter supports. Slack,
Discord and Telegram need tokens (from their app portals or BotFather); WhatsApp
Cloud API, Microsoft Teams, GitHub, GitLab and Bitbucket need cloud credentials
plus a public HTTPS webhook; WhatsApp Web (whatsmeow) needs only a QR scan;
Signal needs no credentials at all, only a running signal-cli-rest-api container
with a registered phone number; the CLI needs nothing.

> 📖 This page is best viewed [on GitHub](https://github.com/lao/botbooter/blob/main/_docs/platforms.md), pkg.go.dev renders the README but not this file.

**Official documentation**

- Slack, [Using Socket Mode](https://docs.slack.dev/apis/events-api/using-socket-mode/)
- Discord, [Developer Portal](https://discord.com/developers/applications) · [Gateway intents](https://discord.com/developers/docs/events/gateway)
- Telegram, [BotFather](https://t.me/BotFather) · [Bot API](https://core.telegram.org/bots/api)
- WhatsApp, [Cloud API](https://developers.facebook.com/docs/whatsapp/cloud-api) · [Webhooks getting started](https://developers.facebook.com/docs/graph-api/webhooks/getting-started)
- Microsoft Teams, [Azure Bot resource](https://learn.microsoft.com/azure/bot-service/abs-quickstart) · [Bot Framework authentication](https://learn.microsoft.com/azure/bot-service/rest-api/bot-framework-rest-connector-authentication)
- GitHub, [Webhooks](https://docs.github.com/webhooks) · [Creating a GitHub App](https://docs.github.com/apps/creating-github-apps) · [Personal access tokens](https://docs.github.com/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens)
- GitLab, [Webhooks](https://docs.gitlab.com/user/project/integrations/webhooks/) · [Webhook events](https://docs.gitlab.com/user/project/integrations/webhook_events/) · [Personal access tokens](https://docs.gitlab.com/user/profile/personal_access_tokens/)
- Bitbucket, [Cloud webhooks](https://support.atlassian.com/bitbucket-cloud/docs/manage-webhooks/) · [Cloud event payloads](https://support.atlassian.com/bitbucket-cloud/docs/event-payloads/) · [API tokens](https://support.atlassian.com/atlassian-account/docs/manage-api-tokens-for-your-atlassian-account/) · [Data Center webhooks](https://confluence.atlassian.com/bitbucketserver/manage-webhooks-938025878.html)
- Signal, [signal-cli-rest-api](https://github.com/bbernhard/signal-cli-rest-api) · [API reference](https://bbernhard.github.io/signal-cli-rest-api/)
- CLI, [`_examples/basic`](../_examples/basic) and the [README Quickstart](../README.md#quickstart)

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

bot, err := slack.New(slack.Config{
	AppToken: os.Getenv("SLACK_APP_TOKEN"), // xapp-…
	BotToken: os.Getenv("SLACK_BOT_TOKEN"), // xoxb-…
})
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

**Reactions.** To make `bot.OnReaction` fire, also subscribe the app to the
`reaction_added` Events API event (step 4) and grant the
[`reactions:read`](https://docs.slack.dev/reference/scopes/reactions.read/)
bot-token scope (step 3). Without both, reactions are never delivered. Slack's
`reaction_added` event carries no bot flag, so the adapter cannot filter other
bots' reactions — guard reply-emitting reaction handlers against loops.

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

**Reactions.** The constructor requests the
[`GUILD_MESSAGE_REACTIONS` and `DIRECT_MESSAGE_REACTIONS`](https://discord.com/developers/docs/events/gateway#list-of-intents)
gateway intents so `bot.OnReaction` fires. Unlike the Message Content Intent
above, these are **standard** (non-privileged) intents — requested in code, with
no toggle to flip in the developer portal. The adapter drops reactions from bot
users in guilds; DM reactions carry no member record, so a bot reactor in a DM is
not filtered.

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

**Reactions.** Reaction updates are delivered in **private chats**, and in
**groups only when the bot is an administrator**. The adapter requests the
`message_reaction` update in addition to the default set, so `bot.OnReaction`
works out of the box once the admin/private-chat requirement is met; reactions
from bot users are dropped.

**Official docs:** [BotFather](https://t.me/BotFather) · [Bot API](https://core.telegram.org/bots/api) · [getUpdates / long polling](https://core.telegram.org/bots/api#getupdates)

---

## WhatsApp

WhatsApp comes in **two flavors, selected by import path**:

- **`botbooter/whatsapp/cloud`** — the official **Meta WhatsApp Business Cloud
  API** (this section). Needs a Meta Business app, a token and a public HTTPS
  webhook; pulls in no third-party dependency.
- **`botbooter/whatsapp/whatsmeow`** — the **WhatsApp Web multidevice protocol**
  via [whatsmeow](https://github.com/tulir/whatsmeow) (see
  [below](#whatsapp-web-whatsmeow)). Pairs with a phone by QR code like WhatsApp
  Web; no Meta account, credentials or webhook, but it drives a linked personal
  account over an unofficial protocol.

Only the flavor you import is compiled into your binary.

### Cloud API flavor

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
import "github.com/lao/botbooter/whatsapp/cloud"

bot, err := cloud.New(cloud.Config{
	Token:         os.Getenv("WA_TOKEN"),
	PhoneNumberID: os.Getenv("WA_PHONE_ID"),
	AppSecret:     os.Getenv("WA_APP_SECRET"),
	VerifyToken:   os.Getenv("WA_VERIFY_TOKEN"),
	Addr:          os.Getenv("WA_ADDR"), // e.g. ":8080"; a bare "8080" is accepted
})
```

Optional `cloud.Config` fields: `Path` (webhook route, default `/webhook`),
`GraphVersion` (Graph API version, default `v23.0`), and `HTTPClient` (the
outbound HTTP client; defaults to a 30-second timeout).

Inbound media arrives **by id, not URL**: `Attachment.ExtraData` holds a
`*cloud.Media`; resolve the bytes with `GET /{media-id}` using your access
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

**Reactions.** Reactions arrive on the **same inbound webhook** as messages, so
no extra configuration is needed — `bot.OnReaction` fires once the webhook is
verified. An empty-emoji reaction is treated as a reaction removal (dropped).

**Official docs:** [Cloud API](https://developers.facebook.com/docs/whatsapp/cloud-api) · [Webhooks](https://developers.facebook.com/docs/graph-api/webhooks/getting-started) · [Webhook components](https://developers.facebook.com/docs/whatsapp/cloud-api/webhooks/components)

### WhatsApp Web (whatsmeow)

The `botbooter/whatsapp/whatsmeow` flavor speaks the **WhatsApp Web multidevice
protocol** via [whatsmeow](https://github.com/tulir/whatsmeow). It links to a
phone exactly like WhatsApp Web — no Meta app, token or webhook:

```go
import wameow "github.com/lao/botbooter/whatsapp/whatsmeow"

bot, err := wameow.New(wameow.Config{}) // zero value works
```

- **First run**: the device store is empty, so `Run` prints a **QR code to
  stderr** (override with `Config.QRCallback`); scan it from *WhatsApp → Linked
  devices* on the phone. Later runs reuse the stored session silently.
- **Session store**: a local SQLite file (`Config.DBPath`, default
  `botbooter-whatsapp-meow.db`, chmod `0600` — it holds the session's crypto keys, so
  treat it like a credential and never commit it). `Config.Container` swaps in a
  caller-managed store (e.g. Postgres); `Config.Client` brings a fully
  configured whatsmeow client.
- **Replies**: the chat JID is `Message.ChannelID`, so
  `b.SendMessageContext(ctx, m.ChannelID, "pong")` just works.
- **Media**: end-to-end encrypted, delivered without a URL. Fetch and decrypt
  with `wameow.Download(ctx, bot, att)`; the raw client is available as
  `wameow.Client(bot)`.
- **One bot per run**: when `New` opened the session store itself, shutdown
  (`Disconnect`, which `Run` performs on exit) closes it, so the same bot
  cannot be run a second time — build a fresh bot with `wameow.New` for each
  run. A caller-supplied `Config.Container` or `Config.Client` is left open
  and stays reusable.
- **Logout**: unlinking the device from the phone ends `Run` with the
  `wameow.ErrLoggedOut` sentinel; reconnecting cannot recover it. Re-link by
  building a fresh bot and running it again to scan a new QR (delete the
  session DB first if the stale session lingers).

**Reactions.** Reactions arrive over the **same websocket** as messages, so
nothing extra needs configuring — `bot.OnReaction` fires automatically. Replies
are **unthreaded**: this adapter has no quoted-reply egress yet, so
`bot.ReplyToMessage` falls back to a plain send. An empty-emoji reaction is
treated as a removal (dropped).

Caveats: this drives a **linked (usually personal) account over the unofficial
Web protocol**; WhatsApp's terms restrict automation on personal accounts, so
prefer the Cloud API flavor for production/business use.

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

go run ./_examples/basic teams
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

Group and channel messages arrive prefixed with the bot's own `<at>Bot</at>`
mention; botbooter strips it from `Message.Content` and excludes the bot's ID
from `Message.MentionedUserIDs`, so anchored patterns (e.g. `^echo`) still
match. Mentions of **other** users are kept in both. Note the divergence: on
Slack and Discord the bot's own mention stays in `Content` and
`MentionedUserIDs`.

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

**Reactions.** The Teams adapter does not surface reaction events, so
`bot.OnReaction` never fires — the echo/command path works, reactions do not.

**Official docs:** [Create a bot for Teams](https://learn.microsoft.com/microsoftteams/platform/bots/how-to/create-a-bot-for-teams) · [Azure Bot quickstart](https://learn.microsoft.com/azure/bot-service/abs-quickstart) · [Bot Framework dashboard](https://dev.botframework.com/bots) · [Connector authentication](https://learn.microsoft.com/azure/bot-service/rest-api/bot-framework-rest-connector-authentication) · [Send & receive messages](https://learn.microsoft.com/azure/bot-service/rest-api/bot-framework-rest-connector-create-messages) · [ngrok](https://ngrok.com/download)

---

## GitHub

botbooter listens for **`issue_comment` webhook events** — comments on issues
*and* on pull-request conversations — and replies by creating issue comments
through the REST API ([go-github](https://github.com/google/go-github)). Like
WhatsApp and Teams, the adapter runs its own webhook server: it binds a local
`Addr`, and you put a **TLS-terminating reverse proxy** (or ngrok while
developing) in front and register the public HTTPS URL as the repository or App
webhook URL.

Two auth modes, configured through the same `github.Config` — set exactly one:

- **PAT mode** — a [personal access token](https://docs.github.com/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens)
  (classic or fine-grained) with *Issues: read & write* on the target
  repositories. The bot comments as the token's user. Quickest to set up; good
  for personal repos and experiments.
- **App mode** — a [GitHub App](https://docs.github.com/apps/creating-github-apps)
  (`AppID` + `InstallationID` + PEM `PrivateKey`). The bot comments as
  `your-app[bot]`, gets its own identity and rate limits, and is the right
  choice for orgs. Tokens are minted and refreshed automatically
  ([ghinstallation](https://github.com/bradleyfalzon/ghinstallation)).

#### Step 1, Create the webhook

On the repository (*Settings → Webhooks → Add webhook*) or on the GitHub App
(*Permissions & events*):

- **Payload URL**: your public `https://…/webhook` endpoint (from ngrok or your
  proxy; you can create the webhook after step 3 if you don't have it yet).
- **Content type**: `application/json`.
- **Secret**: a random string; the adapter **requires** it and rejects requests
  whose `X-Hub-Signature-256` HMAC does not match.
- **Events**: *Issue comments* only (other subscribed events are acked and
  ignored).

For App mode also grant the **Issues: read & write** permission, install the
App on the target repositories, and note the **installation id** (visible in
the installation page URL and in webhook payloads).

#### Step 2, Run the bot

```go
import "github.com/lao/botbooter/github"

// PAT mode:
bot, err := github.New(github.Config{
	Token:         os.Getenv("GITHUB_TOKEN"),          // ghp_… or github_pat_…
	WebhookSecret: os.Getenv("GITHUB_WEBHOOK_SECRET"),
	Addr:          ":8080",                            // a bare "8080" is accepted
})

// App mode:
bot, err := github.New(github.Config{
	AppID:          appID,                              // int64, from the App settings page
	InstallationID: installationID,                     // int64, from the installation
	PrivateKey:     pemBytes,                           // the App's PEM private key
	WebhookSecret:  os.Getenv("GITHUB_WEBHOOK_SECRET"),
	Addr:           ":8080",
})
```

Optional `github.Config` fields: `Path` (webhook route, default `/webhook`),
`HTTPClient` (the outbound API client; defaults to a 30-second timeout), and the
reaction-polling trio — `ReactionPollRepos` (opt-in; empty disables polling,
and the poller only starts when an `OnReaction` handler is registered before
the bot connects; entries are `"owner/name"` or the wildcard `"owner/*"`,
which polls every repo of that owner the credentials can see, minus archived,
re-resolved every ~10 cycles), `ReactionPollInterval` (default 30s; when the
polled repo count at that interval would exceed the poller's request budget of
3000/h the adapter logs a warning and automatically raises the effective
interval to fit) and `ReactionPollNoAutoInterval` (disables that raise — the
warning still logs and the configured interval is honored). Reaction dedup is
in-process only, and only reactions added while the bot is connected are
dispatched — reactions added while it was down are missed. See the reactions
example and CLAUDE.md for the polling contract.

#### Step 3, Expose the local server

```bash
ngrok http 8080
```

Point the webhook's payload URL at `https://…/webhook`, then comment on any
issue or PR in a watched repository. Comment from a **human** account: the bot
ignores its own comments and all other bots' (any `Bot`-typed author), so two
App-mode bots can never reply-loop each other. A PAT-mode bot's comments arrive
as a plain `User`, though — another bot ignores it only when it is that bot's
own account, so two *PAT-mode* bots watching the same repository can still
ping-pong. Prefer App mode when several bots share a repo.

`Message.ChannelID` is `owner/repo#number`, so replies land on the same issue
or PR; `github.RawEvent(m)` returns a `*github.Message` whose `Event` field
carries the full `*gogithub.IssueCommentEvent` (check
`.Event.GetIssue().IsPullRequest()` to tell PRs from issues), and
`github.Client(bot)` exposes the authenticated go-github client for anything
beyond commenting — labels, reactions, checks. `github.Addr(bot)` reports the
bound address when you bind `:0`.

Only the `created` action is dispatched (edits and deletions are acked and
dropped), and every delivery is acked `200` *before* dispatch so slow handlers
cannot make GitHub mark the hook as failing. Attachments are not supported —
comment bodies are markdown.

**Environment variables** (suggested names; the config is plain Go):

| Variable | Value |
|---|---|
| `GITHUB_TOKEN` | PAT for PAT mode (mutually exclusive with the App triple) |
| `GITHUB_APP_ID` / `GITHUB_INSTALLATION_ID` / `GITHUB_PRIVATE_KEY` | App-mode credentials |
| `GITHUB_WEBHOOK_SECRET` | webhook secret; required |
| `GITHUB_ADDR` | local bind address, e.g. `:8080` (a bare port is accepted) |
| `GITHUB_PATH` | optional webhook route; defaults to `/webhook` |

#### No response?

- **`403` on every delivery** (webhook *Recent Deliveries* tab): the webhook
  secret does not match `WebhookSecret`.
- **`200` but no reply**: the comment author is a bot (ignored by design), the
  action was an edit, or no registered pattern matched. In PAT mode also check
  the startup log — if the token cannot resolve `GET /user` the bot shuts down
  (a bot that cannot recognize itself would reply-loop).
- **Replies fail / `Send` errors**: the PAT lacks *Issues: write* on that repo,
  or the App is not installed on it. go-github's typed errors (e.g.
  `*github.RateLimitError`) unwrap with `errors.As`.
- **Endpoint never hit**: the payload URL must be your public
  `https://…/webhook` and reach `Addr` through the proxy; only `POST` is
  accepted on the route.

**Official docs:** [Webhooks](https://docs.github.com/webhooks) · [`issue_comment` event](https://docs.github.com/webhooks/webhook-events-and-payloads#issue_comment) · [Securing webhooks](https://docs.github.com/webhooks/using-webhooks/validating-webhook-deliveries) · [Creating a GitHub App](https://docs.github.com/apps/creating-github-apps) · [Issue comments API](https://docs.github.com/rest/issues/comments) · [ngrok](https://ngrok.com/download)

---

## GitLab

botbooter listens for **Note Hook webhook events** — comments on issues *and* on
merge requests — and replies by creating notes through the REST API
([client-go](https://gitlab.com/gitlab-org/api/client-go)). Like WhatsApp, Teams
and GitHub, the adapter runs its own webhook server: it binds a local `Addr`, and
you put a **TLS-terminating reverse proxy** (or ngrok while developing) in front
and register the public HTTPS URL as the project or group webhook URL.

One auth mode: a **GitLab access token** — [personal](https://docs.gitlab.com/user/profile/personal_access_tokens/),
[project](https://docs.gitlab.com/user/project/settings/project_access_tokens/)
or [group](https://docs.gitlab.com/user/group/settings/group_access_tokens/) —
with the **`api`** scope and at least the **Reporter** role on the target
projects. The bot comments as that token's account, and resolves its own user id
at connect (`GET /user`) so it never replies to itself. Self-hosted instances are
supported through `Config.BaseURL`.

#### Step 1, Create the webhook

On the project (*Settings → Webhooks → Add new webhook*) or on the group
(*Settings → Webhooks*):

- **URL**: your public `https://…/webhook` endpoint (from ngrok or your proxy;
  you can create the webhook after step 3 if you don't have it yet).
- **Secret token**: a random string; the adapter **requires** it and rejects
  with `401` any request whose `X-Gitlab-Token` header does not match — checked
  *before* the body is read.
- **Trigger**: *Comments*, plus *Confidential comments* to also answer on
  confidential issues (GitLab delivers those under a separate trigger). Add
  *Merge request events* / *Push events* only if you set the matching `Config`
  callback; every other enabled trigger is acked and ignored.

#### Step 2, Run the bot

```go
import "github.com/lao/botbooter/gitlab"

bot, err := gitlab.New(gitlab.Config{
	Token:  os.Getenv("GITLAB_TOKEN"),  // glpat-… (personal, project or group)
	Secret: os.Getenv("GITLAB_SECRET"), // the webhook's "Secret token"
	Addr:   ":8080",                    // a bare "8080" is accepted
})
```

Optional `gitlab.Config` fields: `Path` (webhook route, default `/webhook`),
`BaseURL` (a self-hosted instance, e.g. `https://gitlab.example.com`; empty
targets gitlab.com and the `/api/v4` suffix is appended for you), `HTTPClient`
(the outbound API client; defaults to a 30-second timeout), and two callbacks
that route further deliveries on the same endpoint — `OnMergeRequest` (actions
`open`, `reopen`, `update`; MRs the bot authored are dropped) and `OnPush`
(unfiltered — ref filtering is the callback's job). Both run on dispatch
goroutines covered by the shutdown drain, so hand long work off and return.
Unset, those deliveries are acked and dropped. There is **no reaction ingress**
in v1 (GitLab's native Emoji webhook is a future slice), and no attachments —
note bodies are markdown.

#### Step 3, Expose the local server

```bash
ngrok http 8080
```

Point the webhook's URL at `https://…/webhook`, then comment on any issue or
merge request in a watched project. Comment from an account **other than the
bot's**: the adapter drops the bot's own notes by author id, but GitLab note
webhooks carry no bot flag, so *other* bots' notes are still dispatched — guard
reply-emitting handlers if two bots share a project.

`Message.ChannelID` is GitLab-native — `group/project#iid` for an issue,
`group/project!iid` for a merge request — so replies land on the same thread;
`gitlab.RawEvent(m)` returns a `*gitlab.Message` with exactly one of
`IssueComment` (`*gogitlab.IssueCommentEvent`) and `MergeComment`
(`*gogitlab.MergeCommentEvent`) set, and `gitlab.Client(bot)` exposes the
authenticated client-go client for anything beyond commenting — labels, awards,
pipelines. `gitlab.Addr(bot)` reports the bound address when you bind `:0`.

System notes (label/title changes and other automated activity) and edits are
acked and dropped; commit and snippet comments have no reply target in v1 and
are dropped too.

**Internal notes are dropped.** GitLab routes a note to the *Confidential
comments* trigger when the note is internal **or** its noteable is confidential.
Since replies are always plain notes, answering an internal note would publish
the internal thread, so those deliveries are acked and dropped on the note's own
`object_attributes.internal` flag — the adapter reads it straight off the payload
because client-go's typed note events do not expose the field. The noteable is the
second filter: on that trigger, a note on a non-confidential issue, or on any
merge request (merge requests cannot be confidential), is internal by
elimination. A comment on a **confidential issue** still dispatches, and the reply
inherits the issue's restricted audience.

> **Below GitLab 18.6, an internal note on a confidential issue is answered.**
> GitLab added `internal` to the note hook's attributes in **18.6**; every earlier
> release omits it (and none has ever sent the note's pre-rename `confidential`
> spelling — the only `confidential` key in a note delivery describes the
> *noteable*). Without the flag only the noteable filter is left, and it cannot
> tell an internal note on a confidential issue from an ordinary comment there, so
> the note dispatches and the bot's plain-note reply reaches **everyone who can see
> the issue**, not just those who can read internal notes. If that audience matters
> on a pre-18.6 instance, do not enable the *Confidential comments* trigger.

The internal-note drop is the one filtered drop that logs (at `Debug`), so an
acked `200` with no reply can be told apart from a broken handler.

Every authentic delivery is acked `200` — *before* dispatch, and also when it is
dropped, unreadable (over the body cap, or truncated) or shed under a
concurrency bound. An unreadable or unparseable body and a shed delivery are
logged as warnings; the filtered drops above (system, internal, edited and
self-authored notes, commit and snippet comments, events with no callback set)
are silent. GitLab **never re-delivers a failed webhook**, and *consecutive*
failures [auto-disable it](https://docs.gitlab.com/user/project/integrations/webhooks/#auto-disabled-webhooks):
a `4xx`, a `5xx`, a connection timeout and any other HTTP error all count alike,
four in a row disable the hook temporarily (one minute of backoff, doubling on
each further failure up to 24 h, after which it re-enables itself) and forty
disable it permanently until someone re-enables it by hand. One successful
delivery resets the count. So a non-200 would lose that delivery *and* suppress
the ones after it. Slow handlers cannot trip any of this — the ack goes out
before dispatch runs.
The one exception is the `401` on a bad secret token — there, a disabled hook is
the right outcome.

**Environment variables** (suggested names; the config is plain Go):

| Variable | Value |
|---|---|
| `GITLAB_TOKEN` | access token with the `api` scope; required |
| `GITLAB_SECRET` | webhook secret token; required |
| `GITLAB_ADDR` | local bind address, e.g. `:8080` (a bare port is accepted) |
| `GITLAB_PATH` | optional webhook route; defaults to `/webhook` |
| `GITLAB_BASE_URL` | optional self-hosted instance URL; defaults to gitlab.com |

#### No response?

- **`401` on every delivery** (webhook *Recent events* tab): the webhook's
  secret token does not match `Secret`. GitLab treats those as failures and will
  auto-disable the hook rather than keep delivering, so fix the token and send a
  test request to re-enable it.
- **`200` but no reply**: the note's author is the bot itself (ignored by
  design), the note was a system note, an edit or an internal note, or no
  registered pattern matched. Check the bot's log — an oversized/truncated body
  or a saturated concurrency bound is acked `200` and logged as a warning. Also
  check the startup log — if the token cannot resolve `GET /user` the bot shuts
  down (a bot that cannot recognize itself would reply-loop).
- **Replies fail / `Send` errors**: the token lacks the `api` scope or the
  Reporter role on that project. A malformed channel id unwraps as
  `gitlab.ErrBadChannelID` with `errors.Is`.
- **Endpoint never hit**: the webhook URL must be your public
  `https://…/webhook` and reach `Addr` through the proxy; only `POST` is
  accepted on the route. Confidential issues need the *Confidential comments*
  trigger, not just *Comments*.

**Official docs:** [Webhooks](https://docs.gitlab.com/user/project/integrations/webhooks/) · [Comment events](https://docs.gitlab.com/user/project/integrations/webhook_events/#comment-events) · [Personal access tokens](https://docs.gitlab.com/user/profile/personal_access_tokens/) · [Notes API](https://docs.gitlab.com/api/notes/) · [ngrok](https://ngrok.com/download)

---

## Bitbucket

botbooter listens for **pull-request and issue comment webhooks** and replies by
creating comments through the Bitbucket REST API. **One package serves both
flavors**, selected by `Config.BaseURL`: empty targets **Bitbucket Cloud** (REST
2.0 via [ktrysmt/go-bitbucket](https://github.com/ktrysmt/go-bitbucket)), a value
like `https://bitbucket.example.com` targets a **Data Center** instance (REST 1.0
over plain HTTP; the `/rest/api/1.0` prefix is appended for you). Like WhatsApp,
Teams, GitHub and GitLab, the adapter runs its own webhook server: it binds a
local `Addr`, and you put a **TLS-terminating reverse proxy** (or ngrok while
developing) in front and register the public HTTPS URL as the repository webhook.

Issue comments are **Cloud only** — Data Center has no issue tracker — so a
`workspace/repo#N` channel id on a Data Center bot is rejected. Bitbucket has
**no comment reactions** on either flavor, so there is no reaction ingress (a
permanent omission, not a future slice), and comments carry markdown with no
upload channel, so `GetAttachments` returns none.

**Two auth modes, pick exactly one** ([app passwords were removed
platform-wide on 2026-07-28](https://www.atlassian.com/blog/bitbucket/app-passwords-deprecation)
and are never offered):

- **API token** (`Config.Email` + `Config.APIToken`) — an
  [Atlassian API token](https://support.atlassian.com/atlassian-account/docs/manage-api-tokens-for-your-atlassian-account/),
  sent as HTTP Basic `email:token`. This is user-bound, so on Cloud the bot
  resolves its own identity at connect via `GET /2.0/user` and `Config.Self` is
  optional.
- **Access token** (`Config.AccessToken`) — a repository, project or workspace
  access token, sent as Bearer. These are **not bound to a user account**, so
  `GET /2.0/user` returns `401` for them; `Config.Self` is therefore **required**
  in access-token mode. It is also required on **Data Center** (which has no
  whoami endpoint at all). Set it to the bot's own **account UUID** (`{…}`) on
  Cloud, or its **user slug** on Data Center — it is how the adapter drops its own
  comments to avoid reply loops.

#### Step 1, Create the webhook

On the repository (Cloud: *Repository settings → Webhooks → Add webhook*; Data
Center: *Repository settings → Webhooks → Create webhook*):

- **URL**: your public `https://…/webhook` endpoint (from ngrok or your proxy).
- **Secret**: a random string; the adapter **requires** it and verifies the
  `X-Hub-Signature` HMAC-SHA256 of the raw body against it (the same header and
  algorithm on both flavors), rejecting a mismatch with `401`.
- **Triggers**: the **comment** events — Cloud *Pull request → Comment created*
  and *Issue → Comment created*; Data Center *Pull request → Comment added*. Add
  the PR-changed and push triggers (Cloud *Pull request created/updated*,
  *Repository push*; Data Center *Pull request opened/source updated*, *Repository
  push*) only if you set the matching `Config` callback; every other enabled
  trigger is acked and ignored.

#### Step 2, Run the bot

```go
import "github.com/lao/botbooter/bitbucket"

bot, err := bitbucket.New(bitbucket.Config{
	Email:    os.Getenv("BITBUCKET_EMAIL"),     // API-token (Basic) auth …
	APIToken: os.Getenv("BITBUCKET_API_TOKEN"), // … both together
	Secret:   os.Getenv("BITBUCKET_SECRET"),    // the webhook's secret
	Addr:     ":8080",                          // a bare "8080" is accepted
})
```

Optional `bitbucket.Config` fields: `AccessToken` (the Bearer alternative to
`Email`+`APIToken`), `Self` (required for access-token mode and Data Center),
`Path` (webhook route, default `/webhook`), `BaseURL` (a Data Center instance;
empty targets Cloud), `HTTPClient` (the outbound API client; defaults to a
30-second timeout), and two callbacks that route further deliveries on the same
endpoint — `OnPullRequest` (reviewable actions only — Cloud
`pullrequest:created`/`updated`, Data Center `pr:opened`/`pr:from_ref_updated`;
PRs the bot authored are dropped) and `OnPush` (unfiltered — ref filtering is the
callback's job). Both run on dispatch goroutines covered by the shutdown drain, so
hand long work off and return. Unset, those deliveries are acked and dropped.

#### Step 3, Expose the local server

```bash
ngrok http 8080
```

Point the webhook's URL at `https://…/webhook`, then comment on any pull request
(or Cloud issue) in the repository. Comment from an account **other than the
bot's**: the adapter drops the bot's own comments by actor identity, but Bitbucket
comment webhooks carry no bot flag, so *other* bots' comments are still dispatched
— guard reply-emitting handlers if two bots share a repository.

`Message.ChannelID` is `workspace/repo!N` for a pull request or `workspace/repo#N`
for a Cloud issue (Data Center uses the project key: `PROJECTKEY/repo!N`, and a
personal repository's `~USERNAME/repo!N`), so
replies land on the same pull request or issue, and `bitbucket.SendThreaded` (used
by `bot.ReplyToMessage`) nests a reply under a comment's `parent.id` on both
flavors. `bitbucket.RawEvent(m)` returns a `*bitbucket.Message` with exactly one
of `CloudPRComment`, `CloudIssueComment` and `ServerPRComment` set, and
`bitbucket.CloudClient(bot)` exposes the ktrysmt Cloud client for anything beyond
commenting — **it is `nil` on a Data Center bot**, whose replies go out over plain
`net/http`. `bitbucket.Addr(bot)` reports the bound address when you bind `:0`.

**Comment edits are ignored.** An edit arrives under a separate event key
(`pullrequest:comment_updated` / `pr:comment:edited`) the adapter does not route,
so — unlike GitLab — there is no in-payload edit action to filter; only the
*created*/*added* keys dispatch.

Every authentic delivery is acked `200` — *before* dispatch, and also when it is
dropped (self-authored, an edit, an unhandled key, a nil callback), unreadable
(over the body cap, or truncated), unparseable or shed under a concurrency bound.
An unreadable/unparseable body and a shed delivery are logged as warnings; the
other drops are silent. The one exception is the `401` on a bad signature. This
matters because **Bitbucket Cloud times a delivery out at 10 seconds and retries a
failure twice with no backoff** (it does **not** auto-disable a hook like GitLab),
so a slow or `5xx` response turns into duplicate deliveries. A retry after our own
timeout, or a manual *Resend*, can also double-deliver, so **handlers must be
idempotent** — never assume a comment is received exactly once.

**Environment variables** (suggested names; the config is plain Go):

| Variable | Value |
|---|---|
| `BITBUCKET_EMAIL` | Atlassian account email; required with `BITBUCKET_API_TOKEN` |
| `BITBUCKET_API_TOKEN` | Atlassian API token (Basic auth); pair with `BITBUCKET_EMAIL` |
| `BITBUCKET_ACCESS_TOKEN` | repository/project/workspace access token (Bearer); alternative to the two above |
| `BITBUCKET_SELF` | bot's account UUID (Cloud) or user slug (DC); required for access-token mode and Data Center |
| `BITBUCKET_SECRET` | webhook secret for the `X-Hub-Signature` HMAC; required |
| `BITBUCKET_ADDR` | local bind address, e.g. `:8080` (a bare port is accepted) |
| `BITBUCKET_PATH` | optional webhook route; defaults to `/webhook` |
| `BITBUCKET_BASE_URL` | optional Data Center instance URL; empty targets Bitbucket Cloud |

#### No response?

- **`401` on every delivery**: the webhook's secret does not match `Secret`, so
  the `X-Hub-Signature` HMAC fails. Fix the secret on the webhook.
- **`New` returns an error**: both auth modes set, or neither, is
  `bitbucket.ErrAmbiguousAuth`; a missing `Secret`/`Addr`, or a missing `Self` in
  access-token mode or on Data Center, is `bitbucket.ErrMissingConfig`.
- **`200` but no reply**: the comment's author is the bot itself (ignored by
  design), the delivery was an edit or an unhandled key, or no registered pattern
  matched. Check the bot's log — an oversized/truncated/unparseable body or a
  saturated concurrency bound is acked `200` and logged as a warning. On Cloud in
  API-token mode, check the startup log — if `GET /2.0/user` cannot resolve the
  bot shuts down (a bot that cannot recognize itself would reply-loop); in
  access-token mode or on Data Center this is why `Self` is required.
- **Replies fail / `Send` errors**: the token lacks permission on the repository,
  or a `workspace/repo#N` issue channel id was used on a Data Center bot (issues
  are Cloud only) — both unwrap as `bitbucket.ErrBadChannelID` for the channel-id
  case, with `errors.Is`.
- **Endpoint never hit**: the webhook URL must be your public `https://…/webhook`
  and reach `Addr` through the proxy; only `POST` is accepted on the route.

**Official docs:** [Cloud webhooks](https://support.atlassian.com/bitbucket-cloud/docs/manage-webhooks/) · [Cloud event payloads](https://support.atlassian.com/bitbucket-cloud/docs/event-payloads/) · [API tokens](https://support.atlassian.com/atlassian-account/docs/manage-api-tokens-for-your-atlassian-account/) · [Data Center webhooks](https://confluence.atlassian.com/bitbucketserver/manage-webhooks-938025878.html) · [ngrok](https://ngrok.com/download)

---

## Signal

Signal has no official bot API, so botbooter talks to a
**[signal-cli-rest-api](https://github.com/bbernhard/signal-cli-rest-api)
container** instead: the container owns the Signal protocol (registration,
encryption, receiving, sending) and the adapter speaks two channels to it —
inbound messages arrive over the container's **receive WebSocket**
(`/v1/receive/{number}`), replies go out as plain **REST** calls
(`POST /v2/send`). There are **no tokens or API keys** — the only credential is
the phone number registered with the container, and the only things botbooter
needs are the container's URL and that number.

The only host requirement is **Docker** (the container bundles signal-cli and
its Java runtime). Like Telegram, this is a dial-out model: outbound
HTTP/WebSocket only, no public endpoint, port, or webhook to host.

#### Step 1, Run the container

Run it in **`json-rpc` mode** — the adapter's receive WebSocket only exists
there (the default `normal` mode spawns a signal-cli process per request and
serves receive as a plain GET, which the adapter does not use):

```bash
docker run -d --name signal-api --restart=always \
	-p 127.0.0.1:8080:8080 \
	-v $HOME/.local/share/signal-api:/home/.local/share/signal-cli \
	-e MODE=json-rpc \
	bbernhard/signal-cli-rest-api
```

> ⚠️ **The API is unauthenticated**: anyone who can reach it can read and send
> as the bot's account (and fetch its attachments). Publish the port on
> `127.0.0.1` (or a private network) only, and never expose it to the internet.

The volume holds the account state — keep it, or you'll re-register on every
container recreate.

#### Step 2, Register a phone number

The bot needs its **own** phone number (a landline or VoIP number works —
registration can verify by voice call). Either link the container to an
existing Signal account as a secondary device — open

```text
http://127.0.0.1:8080/v1/qrcodelink?device_name=botbooter
```

in a browser and scan the QR code with the Signal app (*Settings → Linked
devices*) — or register the number as a primary device over the API:

```bash
curl -X POST http://127.0.0.1:8080/v1/register/+15550001
curl -X POST http://127.0.0.1:8080/v1/register/+15550001/verify/123-456   # the SMS/voice code
```

If `register` fails with a captcha error, solve one at
[signalcaptchas.org/registration/generate.html](https://signalcaptchas.org/registration/generate.html)
and POST it in the body: `{"captcha": "signalcaptcha://…"}`.

Prefer a dedicated number for a bot: with a linked device the bot shares your
account, and messages you send yourself won't reach it (the adapter drops the
bot's own messages to avoid reply loops).

#### Step 3, Run the bot

```go
import "github.com/lao/botbooter/signal"

bot, err := signal.New(signal.Config{
	BaseURL: os.Getenv("SIGNAL_API_URL"), // e.g. "http://127.0.0.1:8080"; required
	Number:  os.Getenv("SIGNAL_NUMBER"),  // the bot's own E.164 number, e.g. "+15550001"; required
})
```

Optional `signal.Config` fields: `DialTimeout` (bounds the receive-socket
handshake, default 10s), `SendTimeout` (bounds each send round-trip, default
30s), and `HTTPClient` (the outbound REST client).

> ⚠️ **The adapter does not reconnect.** Losing the receive socket — the
> `--restart=always` container being updated, a proxy dropping an idle
> connection — ends `Run` with a `receive socket closed` error. That is unlike
> Slack, Discord, Telegram and whatsmeow, whose SDKs re-dial internally, so the
> usual `log.Fatal(bot.Run(ctx))` exits the process on the first blip. **Run the
> bot under a supervisor** (systemd, Docker restart policy, Kubernetes) or
> re-`Run` it yourself with backoff. A re-dial loop in the adapter is a possible
> follow-up.

Message the bot from **another** account; it drops its own messages.

A sender who does not share their phone number arrives with a UUID instead (the
`sourceNumber` field is absent — Signal's phone-number privacy), so
`Message.UserID`/`ChannelID` is that UUID; whether the container accepts it as a
`/v2/send` recipient depends on its signal-cli version, so a reply may fail with
the container's own error. An envelope carrying no sender identity at all is
dropped with a warning.

**Groups.** A group message arrives with `ChannelID` set to `"group:"+groupID`,
and replying to that `ChannelID` posts back to the group — the adapter converts
the id to the REST API's group-recipient form internally, so handlers need no
special casing. The raw group id is on `signal.RawMessage(m).GroupID`.

**Attachments.** The container delivers attachments by id and serves the bytes
itself, so `Attachment.URL` points at its `/v1/attachments/{id}` endpoint —
fetch it with any HTTP client (the URL is only as reachable, and as private, as
the container). `Attachment.ExtraData` carries a `*signal.Attachment` with the
id, content type, filename and size.

**Environment variables** (read by the bundled example):

| Variable | Value |
|---|---|
| `SIGNAL_API_URL` | container base URL from step 1, e.g. `http://127.0.0.1:8080` |
| `SIGNAL_NUMBER` | the bot's own number registered in step 2 (`+15550001`) |

#### No messages?

- **`Connect` fails**: the container isn't running, `SIGNAL_API_URL` is wrong,
  or the container isn't in `json-rpc` mode — the receive WebSocket only exists
  there (check `docker logs signal-api`).
- **`Send` fails**: the REST call surfaces the container's error message —
  typically an unregistered number, or a number that doesn't match the
  registered account.
- **Nothing arrives**: you're messaging from the bot's own account (drop-own
  filtering), or the number never finished registration/linking — hit
  `http://127.0.0.1:8080/v1/accounts` to list registered accounts.

**Reactions.** The Signal adapter does not surface reaction events, so
`bot.OnReaction` never fires — the echo/command path works, reactions do not.

**Official docs:** [signal-cli-rest-api](https://github.com/bbernhard/signal-cli-rest-api) · [API reference](https://bbernhard.github.io/signal-cli-rest-api/) · [Execution modes](https://github.com/bbernhard/signal-cli-rest-api#execution-modes) · [signal-cli](https://github.com/AsamK/signal-cli)

---

## CLI

No credentials, no portal, no setup. The CLI adapter reads from stdin and
writes to stdout, so it's the fastest way to develop and test handlers:

```bash
go run ./_examples/basic            # CLI mode (default)
```

**Environment variables:** none.

A terminal has no real upload channel, so the CLI treats any local file path
in a message as an attachment, see [Attachments](../README.md#attachments)
in the README. **Use it with trusted local input only.**

**Official docs:** the bundled [`_examples/basic`](../_examples/basic) and the
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
| **WhatsApp (Cloud API)** | `m.ID` | a **quote message id** | `context.message_id` (a quoted reply) | plain send (no `context`) |
| **WhatsApp (Web / whatsmeow)** | — (options ignored) | — | — | always a plain channel send (quoted replies are a possible follow-up) |
| **Teams** | — (options ignored) | — | — | always a plain channel send |
| **GitHub** | — (options ignored) | — | — | always a plain issue comment (issue comment threads are flat — a reply already lands in the conversation) |
| **GitLab** | — (options ignored) | — | — | always a plain note (threading via GitLab discussions — the payload's `discussion_id` — is a deliberate v1 omission) |
| **Bitbucket** | `m.ID` (the comment id) | a **comment id** | `parent.id` on the new comment (Cloud and Data Center) | plain top-level comment |
| **Signal** | — (options ignored) | — | — | always a plain channel send (quoted replies are a possible follow-up) |
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
(WhatsApp Web/whatsmeow, Teams, GitHub, GitLab, Signal, CLI) **or** the anchor
resolves to nothing — it never fails just because
a message can't be threaded. The one loud exception is an explicit
`WithThreadID` that a platform can't use (an id that isn't a positive message id
on Telegram), which returns an error rather than silently dropping. `Reply` returns an error only
when the bot has no adapter or `m` is `nil`.

To reach beyond these normalized semantics (Slack broadcast replies, message
edits, etc.), drop to the raw SDK client via the per-platform accessors — see
[Raw platform access](../README.md#raw-platform-access).

---

[← Back to the README](../README.md)
