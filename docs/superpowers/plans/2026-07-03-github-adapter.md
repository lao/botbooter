# GitHub Adapter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a GitHub platform adapter to botbooter — issue-ops bots that receive `issue_comment` webhooks and reply as issue/PR comments.

**Architecture:** Third copy of the webhook-adapter pattern (WhatsApp/Teams): `internal/github` implements `core.Adapter` with its own HTTP server, HMAC verification, ack-then-dispatch, and in-flight drain; a thin public `github/` facade exposes `New`, `Client`, `RawEvent`, `Addr`. Outbound goes through one `*github.Client` (go-github SDK); PAT vs GitHub App auth is resolved at the `http.RoundTripper` level (ghinstallation).

**Tech Stack:** Go 1.23+, `google/go-github` (SDK), `bradleyfalzon/ghinstallation/v2` (App transport). Tests: stdlib + in-repo `internal/asserts` only.

**Spec:** `docs/superpowers/specs/2026-07-03-github-adapter-design.md` — authoritative on every behavior below.

## Global Constraints

- Go 1.23; module `github.com/lao/botbooter`; pre-1.0.
- Tests use `internal/asserts` helpers (`Equal`, `NotNil`, `Error`, `ErrorIs`, `NoError`, `True`, `False`) — **never testify**.
- All tests hermetic: no real network I/O; `httptest` only.
- `botbooter.go` stays SDK-free (type aliases + consts + sentinels only).
- Facade packages stay thin — logic lives in `internal/github`.
- Mirror WhatsApp/Teams scaffolding byte-for-byte where concerns are shared (drain, timeouts, teardown identity checks).
- Every task ends with `go build ./... && go test -race ./...` green before commit; final gate is `make all`.
- Commit after every task with a Conventional Commits message. Do NOT add a Claude co-author trailer (repo history has none).
- Constants: `maxRequestBytes = 1 << 20`, `shutdownTimeout = 5 * time.Second`, `drainTimeout = 5 * time.Second`, `defaultPath = "/webhook"`.

---

### Task 1: Dependencies + API shape verification

**Files:**
- Modify: `go.mod`, `go.sum` (via `go get`)

**Interfaces:**
- Produces: `github.com/google/go-github/vNN/github` and `github.com/bradleyfalzon/ghinstallation/v2` available to `internal/github`. Records the actual major version `NN` used in all later import paths.

The spec was written against go-github **v88** with a functional-options `NewClient` returning `(*Client, error)`. Verify what the fetched version actually exposes and note it — every later task imports `gogithub "github.com/google/go-github/vNN/github"` with the version you record here.

- [ ] **Step 1: Fetch modules**

```bash
go get github.com/google/go-github/v88@latest || go get github.com/google/go-github/v88
```

If v88 does not resolve, find the latest major: check https://pkg.go.dev/github.com/google/go-github and `go get` that. Then:

```bash
go get github.com/bradleyfalzon/ghinstallation/v2@latest
go mod tidy
```

- [ ] **Step 2: Verify API shapes**

```bash
go doc github.com/google/go-github/v88/github.NewClient
go doc github.com/google/go-github/v88/github.Client.WithAuthToken
go doc github.com/google/go-github/v88/github.ValidateSignature
go doc github.com/google/go-github/v88/github.ParseWebHook
go doc github.com/google/go-github/v88/github.WebHookType
go doc github.com/bradleyfalzon/ghinstallation/v2.New
go doc github.com/bradleyfalzon/ghinstallation/v2.NewAppsTransport
```

Record: (a) does `NewClient` take functional options and return `(*Client, error)`, or the classic `NewClient(httpClient *http.Client) *Client` + chained `.WithAuthToken(token) *Client`? (b) exact name of the test-URL helper (`WithURLs` option vs `client.WithEnterpriseURLs(base, upload)`). **Adapt the client-construction code in Task 3 and test code in Tasks 5/7 to the real API** — the plan shows both variants where they diverge.

Also check which `golang-jwt/jwt` major ghinstallation pulled (`go mod graph | grep golang-jwt`) — needed for Task 9's isolation row.

- [ ] **Step 3: Build + commit**

```bash
go build ./... && go test -race ./...
git add go.mod go.sum
git commit -m "build: add go-github and ghinstallation for the GitHub adapter"
```

---

### Task 2: Core BotType + root re-export

**Files:**
- Modify: `internal/core/core.go` (BotType iota block ~line 29, `String()` ~line 37)
- Modify: `internal/core/core_test.go` (~line 23, the BotType string test)
- Modify: `botbooter.go` (const block)
- Test: `internal/core/core_test.go`

**Interfaces:**
- Produces: `core.GitHubBotType` (last iota value, `String() == "github"`), `botbooter.GitHubBotType`.

- [ ] **Step 1: Write the failing test** — in `internal/core/core_test.go`, extend the existing BotType string test (next to the `TeamsBotType` line):

```go
asserts.Equal(t, GitHubBotType.String(), "github", "GitHub string")
```

- [ ] **Step 2: Run to verify it fails**

```bash
go test -run TestBotType ./internal/core/
```

Expected: FAIL — `undefined: GitHubBotType` (compile error counts as the red step).

- [ ] **Step 3: Implement** — in `internal/core/core.go`, append to the iota block (order matters, values are ordinal — always append at the end):

```go
	TeamsBotType
	GitHubBotType
)
```

and to `String()`:

```go
	case GitHubBotType:
		return "github"
```

In `botbooter.go`, append inside the `Supported bot types` const block:

```go
	GitHubBotType = core.GitHubBotType
```

- [ ] **Step 4: Run tests**

```bash
go test -race ./internal/core/ ./
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/core/core.go internal/core/core_test.go botbooter.go
git commit -m "feat(core): add GitHubBotType"
```

---

### Task 3: internal/github — Config, validation, client construction

**Files:**
- Create: `internal/github/github.go`
- Test: `internal/github/github_test.go`

**Interfaces:**
- Produces:
  - `type Config struct { Token string; AppID, InstallationID int64; PrivateKey []byte; WebhookSecret, Addr, Path string; HTTPClient *http.Client }`
  - `var ErrMissingConfig`, `var ErrAmbiguousAuth` (sentinels, `errors.Is`-checkable)
  - `func New(cfg Config) (*core.Bot, error)` — builds adapter, wraps in `core.New(core.GitHubBotType, a)`
  - `func newAdapter(cfg Config) (*adapter, error)` — validation + client construction (used directly by tests)
  - `type adapter struct` with fields `cfg Config; client *gogithub.Client; baseTransport http.RoundTripper; mu sync.Mutex; selfID int64; selfLogin string; srv *http.Server; boundAddr string; detachedCancel context.CancelFunc; inflight atomic.Int64`
  - `func Addr(b *core.Bot) string`, `func Client(b *core.Bot) *gogithub.Client` — package-level accessors via `core.AdapterAs[*adapter]`
- Consumes: `core.GitHubBotType` (Task 2), modules (Task 1).

Note: the adapter's `Connect`/`Disconnect`/`Send`/`Attachments` don't exist yet — to keep this task compiling, add stub methods in this task's implementation step (they are replaced with real code in Tasks 5–7):

```go
func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error { return nil }
func (a *adapter) Disconnect() error                                        { return nil }
func (a *adapter) Send(ctx context.Context, channelID, text string) error   { return nil }
func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error)   { return nil, nil }
```

- [ ] **Step 1: Write the failing tests** — `internal/github/github_test.go`:

```go
package github

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// testKeyPEM returns a freshly generated PKCS#1 RSA private key in PEM form,
// the format GitHub issues for App keys and ghinstallation parses.
func testKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	asserts.NoError(t, err, "generate test RSA key")
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func patConfig() Config {
	return Config{Token: "ghp_test", WebhookSecret: "hook-secret", Addr: "127.0.0.1:0"}
}

func appConfig(t *testing.T) Config {
	return Config{AppID: 7, InstallationID: 11, PrivateKey: testKeyPEM(t),
		WebhookSecret: "hook-secret", Addr: "127.0.0.1:0"}
}

func TestNewAdapter_Validation(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want error
	}{
		{"NoAuth", Config{WebhookSecret: "s", Addr: ":0"}, ErrMissingConfig},
		{"BothModes", Config{Token: "t", AppID: 1, InstallationID: 2, PrivateKey: []byte("k"), WebhookSecret: "s", Addr: ":0"}, ErrAmbiguousAuth},
		{"PartialAppTriple", Config{AppID: 1, WebhookSecret: "s", Addr: ":0"}, ErrMissingConfig},
		{"MissingSecret", Config{Token: "t", Addr: ":0"}, ErrMissingConfig},
		{"MissingAddr", Config{Token: "t", WebhookSecret: "s"}, ErrMissingConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newAdapter(tc.cfg)
			asserts.ErrorIs(t, err, tc.want, tc.name)
		})
	}
}

func TestNewAdapter_Normalization(t *testing.T) {
	cfg := patConfig()
	cfg.Addr = "8080"
	cfg.Path = "hooks"
	a, err := newAdapter(cfg)

	asserts.NoError(t, err, "valid PAT config")
	asserts.Equal(t, a.cfg.Addr, ":8080", "bare port gets a colon")
	asserts.Equal(t, a.cfg.Path, "/hooks", "path gets a leading slash")
	asserts.NotNil(t, a.cfg.HTTPClient, "default HTTP client applied")
	asserts.NotNil(t, a.client, "go-github client built")
}

func TestNewAdapter_DefaultPath(t *testing.T) {
	a, err := newAdapter(patConfig())

	asserts.NoError(t, err, "valid PAT config")
	asserts.Equal(t, a.cfg.Path, "/webhook", "default path")
}

// Guards the ghinstallation nil-RoundTripper panic: App mode with the default
// (nil-Transport) HTTPClient must normalize to http.DefaultTransport.
func TestNewAdapter_AppModeDefaultTransport(t *testing.T) {
	a, err := newAdapter(appConfig(t))

	asserts.NoError(t, err, "App mode with default HTTPClient")
	asserts.NotNil(t, a.client, "client built")
	asserts.NotNil(t, a.baseTransport, "normalized inner transport stored")
	asserts.True(t, a.baseTransport == http.DefaultTransport, "nil Transport normalizes to http.DefaultTransport")
}

func TestNewAdapter_AppModeBadKey(t *testing.T) {
	cfg := appConfig(t)
	cfg.PrivateKey = []byte("not a pem key")
	_, err := newAdapter(cfg)

	asserts.Error(t, err, "unparseable private key should error")
}

func TestNew_BotType(t *testing.T) {
	bot, err := New(patConfig())

	asserts.NoError(t, err, "new GitHub bot")
	asserts.Equal(t, bot.BotType, core.GitHubBotType, "bot type should be GitHub")
}

func TestAddr_NotConnected(t *testing.T) {
	bot, err := New(patConfig())
	asserts.NoError(t, err, "new GitHub bot")
	asserts.Equal(t, Addr(bot), "", "Addr is empty before Connect")
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test -race ./internal/github/
```

Expected: FAIL (package does not compile — nothing defined yet).

- [ ] **Step 3: Implement** — `internal/github/github.go`:

```go
// Package github is the GitHub adapter for botbooter. It receives issue and PR
// comments as issue_comment webhook events over an inbound HTTP server and
// replies by creating issue comments through the GitHub REST API (go-github).
// It implements core.Adapter.
//
// Like the WhatsApp and Teams adapters, Connect binds a listener and serves
// until the run context is canceled; Disconnect shuts it down and drains
// in-flight dispatch. Bind a local Addr, put a TLS-terminating proxy in front,
// and register the public HTTPS URL as the repository or App webhook URL
// (content type application/json, events: issue_comment, with a secret).
//
// Implementation split (mirrors Teams): github.go (config, auth wiring,
// accessors), server.go (webhook lifecycle), send.go (replies), message.go
// (payload mapping).
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gogithub "github.com/google/go-github/v88/github"
	"github.com/lao/botbooter/internal/core"
)

const (
	defaultPath = "/webhook"

	// The endpoint is public; cap bodies against memory exhaustion. Real
	// issue_comment payloads are tens of KB at most.
	maxRequestBytes = 1 << 20 // 1 MiB

	shutdownTimeout = 5 * time.Second
	drainTimeout    = 5 * time.Second
)

// ErrMissingConfig is returned by New when a required Config field is empty.
var ErrMissingConfig = errors.New("github: missing required config field")

// ErrAmbiguousAuth is returned by New when both PAT and App auth are configured.
var ErrAmbiguousAuth = errors.New("github: configure either Token or AppID/InstallationID/PrivateKey, not both")

// Config configures a GitHub bot. Exactly one auth mode must be set: Token
// (PAT mode) or the AppID/InstallationID/PrivateKey triple (App mode).
type Config struct {
	// Token is a personal access token (classic or fine-grained) for PAT mode.
	// The bot posts comments as the token's user.
	Token string

	// AppID is the GitHub App ID for App mode.
	AppID int64
	// InstallationID is the App installation to act as; it is also visible in
	// webhook payloads and on the installation settings page.
	InstallationID int64
	// PrivateKey is the App's RSA private key, PEM-encoded.
	PrivateKey []byte

	// WebhookSecret verifies the X-Hub-Signature-256 HMAC on inbound webhook
	// requests. Required: without it the endpoint would accept spoofed payloads.
	WebhookSecret string
	// Addr is the local TCP address the webhook server binds, e.g. ":8080". A
	// bare port ("8080") is accepted as shorthand for ":8080".
	Addr string
	// Path is the webhook route; it defaults to /webhook.
	Path string

	// HTTPClient is the base client for outbound GitHub API calls; a default
	// client with a 30s timeout is used when nil. In App mode its Transport
	// (http.DefaultTransport when nil) becomes the inner transport of the
	// ghinstallation token-refreshing transport.
	HTTPClient *http.Client
}

type adapter struct {
	cfg    Config
	client *gogithub.Client
	// baseTransport is the normalized inner RoundTripper (never nil); Connect
	// reuses it for the one-shot App-JWT client during self-identity resolution.
	baseTransport http.RoundTripper

	mu sync.Mutex
	// selfID/selfLogin identify the bot's own account for reply-loop
	// prevention. Written by Connect under mu (same critical section as srv),
	// read by the handler under mu; re-resolved on every Connect, never
	// cleared on Disconnect (stale values are harmless with no server up).
	selfID    int64
	selfLogin string
	srv       *http.Server
	// boundAddr is the listener's resolved address, so a cfg.Addr of ":0" is
	// recoverable via Addr. Set with srv, cleared with it.
	boundAddr string
	// detachedCancel aborts the current connection's dispatch goroutines. Each
	// Connect derives one detached, cancelable context and threads it through
	// the handler closure, so only the cancel is shared state. Disconnect calls
	// it after the drain window so a stuck handler cannot leak, and clears it
	// only when a reconnect has not already installed a newer connection.
	detachedCancel context.CancelFunc
	inflight       atomic.Int64
}

// New creates a GitHub bot. It returns ErrMissingConfig if a required field is
// absent and ErrAmbiguousAuth if both auth modes are set, and otherwise applies
// defaults for Path and HTTPClient. The webhook server is not started until the
// bot connects.
func New(cfg Config) (*core.Bot, error) {
	a, err := newAdapter(cfg)
	if err != nil {
		return nil, err
	}
	return core.New(core.GitHubBotType, a), nil
}

// Addr returns the address the bot's webhook listener is bound to (host:port),
// or "" if b is not a GitHub bot or is not currently connected. It lets a
// caller that passed cfg.Addr ":0" discover the OS-assigned port.
func Addr(b *core.Bot) string {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.boundAddr
	}
	return ""
}

// Client returns the underlying go-github client, or nil if b is not a GitHub
// bot. Use it for API calls beyond the adapter's send path (labels, reactions,
// checks); it is safe for concurrent use.
func Client(b *core.Bot) *gogithub.Client {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		return a.client
	}
	return nil
}

func newAdapter(cfg Config) (*adapter, error) {
	patMode := cfg.Token != ""
	appMode := cfg.AppID != 0 || cfg.InstallationID != 0 || len(cfg.PrivateKey) > 0
	switch {
	case patMode && appMode:
		return nil, ErrAmbiguousAuth
	case !patMode && !appMode:
		return nil, fmt.Errorf("%w: Token or AppID/InstallationID/PrivateKey is required", ErrMissingConfig)
	case appMode && (cfg.AppID == 0 || cfg.InstallationID == 0 || len(cfg.PrivateKey) == 0):
		return nil, fmt.Errorf("%w: App mode needs AppID, InstallationID and PrivateKey", ErrMissingConfig)
	}
	if cfg.WebhookSecret == "" || cfg.Addr == "" {
		return nil, fmt.Errorf("%w: WebhookSecret and Addr are required", ErrMissingConfig)
	}
	// A bare port ("8080") is shorthand for ":8080".
	if _, err := strconv.Atoi(cfg.Addr); err == nil {
		cfg.Addr = ":" + cfg.Addr
	}
	if cfg.Path == "" {
		cfg.Path = defaultPath
	}
	// A pattern without a leading slash panics ServeMux at Connect.
	if !strings.HasPrefix(cfg.Path, "/") {
		cfg.Path = "/" + cfg.Path
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	// ghinstallation calls its inner transport without a nil check, and the
	// default HTTPClient has a nil Transport — normalize explicitly.
	baseTransport := cfg.HTTPClient.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}

	a := &adapter{cfg: cfg, baseTransport: baseTransport}
	if patMode {
		client, err := gogithub.NewClient(
			gogithub.WithHTTPClient(cfg.HTTPClient),
			gogithub.WithAuthToken(cfg.Token),
		)
		if err != nil {
			return nil, fmt.Errorf("github: build client: %w", err)
		}
		a.client = client
	} else {
		itr, err := ghinstallation.New(baseTransport, cfg.AppID, cfg.InstallationID, cfg.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("github: build installation transport: %w", err)
		}
		client, err := gogithub.NewClient(gogithub.WithHTTPClient(
			&http.Client{Transport: itr, Timeout: cfg.HTTPClient.Timeout},
		))
		if err != nil {
			return nil, fmt.Errorf("github: build client: %w", err)
		}
		a.client = client
	}
	return a, nil
}
```

**API-variant note (from Task 1):** if the fetched go-github major uses the classic API instead of functional options, the two client constructions become:

```go
// PAT:
a.client = gogithub.NewClient(cfg.HTTPClient).WithAuthToken(cfg.Token)
// App:
a.client = gogithub.NewClient(&http.Client{Transport: itr, Timeout: cfg.HTTPClient.Timeout})
```

with no error returns. Use whichever compiles per `go doc`; keep the surrounding structure identical.

Also add the four temporary stub methods from the Interfaces block above so `core.New` type-checks.

- [ ] **Step 4: Run tests**

```bash
go test -race ./internal/github/
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/github/
git commit -m "feat(github): adapter config, validation and dual-mode client construction"
```

---

### Task 4: message.go — raw message type + core.Message mapping

**Files:**
- Create: `internal/github/message.go`
- Test: `internal/github/message_test.go`

**Interfaces:**
- Produces:
  - `type Message struct { Event *gogithub.IssueCommentEvent }`
  - `func RawEvent(m *core.Message) (*Message, bool)`
  - `func toMessage(event *gogithub.IssueCommentEvent) *core.Message`
- Consumes: nothing from Tasks 1–3 beyond the package existing.

- [ ] **Step 1: Write the failing test** — `internal/github/message_test.go`:

```go
package github

import (
	"encoding/json"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v88/github"
	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// issueCommentCreated is a trimmed real-shaped issue_comment payload.
const issueCommentCreated = `{
  "action": "created",
  "issue": {"number": 42},
  "comment": {
    "id": 1234567890,
    "body": "/deploy staging",
    "created_at": "2026-07-03T10:00:00Z",
    "user": {"id": 58394, "login": "octocat", "type": "User"}
  },
  "repository": {"full_name": "lao/botbooter"},
  "sender": {"id": 58394, "login": "octocat", "type": "User"}
}`

func parseEvent(t *testing.T, payload string) *gogithub.IssueCommentEvent {
	t.Helper()
	var ev gogithub.IssueCommentEvent
	asserts.NoError(t, json.Unmarshal([]byte(payload), &ev), "parse test payload")
	return &ev
}

func TestToMessage(t *testing.T) {
	got := toMessage(parseEvent(t, issueCommentCreated))

	asserts.Equal(t, got.ID, "1234567890", "ID is the comment id")
	asserts.Equal(t, got.UserID, "58394", "UserID is the author's numeric id")
	asserts.Equal(t, got.AuthorName, "octocat", "AuthorName is the login")
	asserts.Equal(t, got.ChannelID, "lao/botbooter#42", "ChannelID round-trips into Send")
	asserts.Equal(t, got.Content, "/deploy staging", "Content is the comment body")
	asserts.True(t, got.Timestamp.Equal(time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)), "Timestamp from created_at")
}

func TestRawEvent(t *testing.T) {
	ev := parseEvent(t, issueCommentCreated)
	m := toMessage(ev)

	raw, ok := RawEvent(m)
	asserts.True(t, ok, "raw event present")
	asserts.True(t, raw.Event == ev, "raw carries the original event")

	_, ok = RawEvent(&core.Message{Raw: "not ours"})
	asserts.False(t, ok, "foreign raw payload reports false")
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test -race -run 'TestToMessage|TestRawEvent' ./internal/github/
```

Expected: FAIL — `undefined: toMessage`, `undefined: RawEvent`.

- [ ] **Step 3: Implement** — `internal/github/message.go`:

```go
package github

import (
	"strconv"

	gogithub "github.com/google/go-github/v88/github"
	"github.com/lao/botbooter/internal/core"
)

// Message is the typed raw payload stored in core.Message.Raw for GitHub bots.
// Consumers can distinguish PR comments from issue comments via
// Event.GetIssue().IsPullRequest().
type Message struct {
	Event *gogithub.IssueCommentEvent
}

// RawEvent returns the typed issue_comment event carried on m, reporting
// whether m originated from GitHub.
func RawEvent(m *core.Message) (*Message, bool) {
	v, ok := m.Raw.(*Message)
	return v, ok
}

// toMessage maps an issue_comment event into the platform-agnostic message.
// ChannelID is "owner/repo#number", exactly what Send's parseChannelID accepts,
// so bot.Send(ctx, msg.ChannelID, ...) replies on the same issue or PR.
func toMessage(event *gogithub.IssueCommentEvent) *core.Message {
	comment := event.GetComment()
	return &core.Message{
		ID:         strconv.FormatInt(comment.GetID(), 10),
		UserID:     strconv.FormatInt(comment.GetUser().GetID(), 10),
		AuthorName: comment.GetUser().GetLogin(),
		ChannelID:  event.GetRepo().GetFullName() + "#" + strconv.Itoa(event.GetIssue().GetNumber()),
		Content:    comment.GetBody(),
		Timestamp:  comment.GetCreatedAt().Time,
		Raw:        &Message{Event: event},
	}
}
```

(If `GetCreatedAt()` returns `time.Time` directly instead of a `Timestamp` struct in the fetched version, drop the `.Time`.)

- [ ] **Step 4: Run tests**

```bash
go test -race ./internal/github/
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/github/message.go internal/github/message_test.go
git commit -m "feat(github): map issue_comment events to core messages"
```

---

### Task 5: send.go — parseChannelID + Send

**Files:**
- Create: `internal/github/send.go`
- Test: `internal/github/send_test.go`
- Modify: `internal/github/github.go` (delete the `Send` stub)

**Interfaces:**
- Produces:
  - `var ErrBadChannelID`
  - `func parseChannelID(channelID string) (owner, repo string, number int, err error)`
  - `func (a *adapter) Send(ctx context.Context, channelID, text string) error`
- Consumes: `adapter.client` (Task 3).

Test repointing: go-github clients can be pointed at an `httptest.Server`. With the functional-options API use `gogithub.NewClient(gogithub.WithHTTPClient(...), gogithub.WithURLs(srv.URL+"/", srv.URL+"/"))` (check the exact option name from Task 1); with the classic API use `client, _ := gogithub.NewClient(nil).WithEnterpriseURLs(srv.URL+"/", srv.URL+"/")`. Wrap whichever works in a small test helper `testClient(t, srv) *gogithub.Client` and reuse it in Task 7.

- [ ] **Step 1: Write the failing tests** — `internal/github/send_test.go`:

```go
package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v88/github"
	"github.com/lao/botbooter/internal/asserts"
)

// TestSend_PATAuthorizationHeader asserts the PAT wiring: the client built by
// newAdapter must send the token on outbound calls.
func TestSend_PATAuthorizationHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 1}`))
	}))
	defer srv.Close()

	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	// Rebuild the adapter's own client against the test server, preserving its
	// auth transport chain. With the functional-options API:
	client, err := gogithub.NewClient(
		gogithub.WithHTTPClient(a.cfg.HTTPClient),
		gogithub.WithAuthToken(a.cfg.Token),
		gogithub.WithURLs(srv.URL+"/", srv.URL+"/"),
	)
	asserts.NoError(t, err, "repoint client")
	a.client = client

	asserts.NoError(t, a.Send(context.Background(), "lao/botbooter#42", "hi"), "send")
	asserts.True(t, strings.Contains(gotAuth, "ghp_test"), "Authorization carries the PAT, got "+gotAuth)
}
// (Classic-API variant: a.client = gogithub.NewClient(nil).WithAuthToken(a.cfg.Token)
// then client.WithEnterpriseURLs(srv.URL, srv.URL) — adapt per Task 1.)

func TestParseChannelID(t *testing.T) {
	cases := []struct {
		in         string
		owner      string
		repo       string
		number     int
		wantErr    bool
	}{
		{"lao/botbooter#42", "lao", "botbooter", 42, false},
		{"a/b#12", "a", "b", 12, false},
		{"owner/repo", "", "", 0, true},   // no #number
		{"owner#1", "", "", 0, true},      // no /repo
		{"owner/repo#0", "", "", 0, true}, // non-positive number
		{"owner/repo#abc", "", "", 0, true},
		{"/repo#1", "", "", 0, true},  // empty owner
		{"owner/#1", "", "", 0, true}, // empty repo
		{"", "", "", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			owner, repo, number, err := parseChannelID(tc.in)
			if tc.wantErr {
				asserts.ErrorIs(t, err, ErrBadChannelID, "malformed id")
				return
			}
			asserts.NoError(t, err, "valid id")
			asserts.Equal(t, owner, tc.owner, "owner")
			asserts.Equal(t, repo, tc.repo, "repo")
			asserts.Equal(t, number, tc.number, "number")
		})
	}
}

// testClient builds a go-github client pointed at srv. Adapt the construction
// to the API variant recorded in Task 1.
func testClient(t *testing.T, srv *httptest.Server) *gogithub.Client {
	t.Helper()
	client, err := gogithub.NewClient(
		gogithub.WithHTTPClient(srv.Client()),
		gogithub.WithURLs(srv.URL+"/", srv.URL+"/"),
	)
	asserts.NoError(t, err, "build test client")
	return client
}

func TestSend_PostsComment(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 1}`))
	}))
	defer srv.Close()

	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)

	asserts.NoError(t, a.Send(context.Background(), "lao/botbooter#42", "done"), "send")
	asserts.True(t, strings.HasSuffix(gotPath, "/repos/lao/botbooter/issues/42/comments"), "comment endpoint, got "+gotPath)
	var payload struct {
		Body string `json:"body"`
	}
	asserts.NoError(t, json.Unmarshal([]byte(gotBody), &payload), "request body is JSON")
	asserts.Equal(t, payload.Body, "done", "comment body")
}

func TestSend_BadChannelID(t *testing.T) {
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")

	asserts.ErrorIs(t, a.Send(context.Background(), "not-a-channel", "x"), ErrBadChannelID, "malformed channel id")
}

func TestSend_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message": "rate limited"}`))
	}))
	defer srv.Close()

	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)

	err = a.Send(context.Background(), "lao/botbooter#42", "x")
	asserts.Error(t, err, "API failure surfaces")
	asserts.True(t, strings.Contains(err.Error(), "lao/botbooter#42"), "error names the channel")
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test -race -run 'TestParseChannelID|TestSend' ./internal/github/
```

Expected: FAIL — `undefined: parseChannelID`, `undefined: ErrBadChannelID` (and the Send stub returns nil, failing assertions once those compile).

- [ ] **Step 3: Implement** — `internal/github/send.go`, and delete the `Send` stub from `github.go`:

```go
package github

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	gogithub "github.com/google/go-github/v88/github"
)

// ErrBadChannelID is returned by Send when channelID is not "owner/repo#number".
var ErrBadChannelID = errors.New(`github: channel ID must be "owner/repo#number"`)

// parseChannelID splits "owner/repo#number" on the last '#'. The number must be
// a positive integer and both owner and repo must be non-empty.
func parseChannelID(channelID string) (owner, repo string, number int, err error) {
	hash := strings.LastIndex(channelID, "#")
	if hash < 0 {
		return "", "", 0, fmt.Errorf("%w: %q", ErrBadChannelID, channelID)
	}
	number, convErr := strconv.Atoi(channelID[hash+1:])
	if convErr != nil || number <= 0 {
		return "", "", 0, fmt.Errorf("%w: %q", ErrBadChannelID, channelID)
	}
	owner, repo, ok := strings.Cut(channelID[:hash], "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", 0, fmt.Errorf("%w: %q", ErrBadChannelID, channelID)
	}
	return owner, repo, number, nil
}

// Send posts text as a comment on the issue or PR identified by channelID
// ("owner/repo#number" — PRs are issues for commenting purposes). API errors
// are %w-wrapped, so callers can unwrap go-github's typed errors (e.g.
// *github.RateLimitError) with errors.As.
func (a *adapter) Send(ctx context.Context, channelID, text string) error {
	owner, repo, number, err := parseChannelID(channelID)
	if err != nil {
		return err
	}
	_, _, err = a.client.Issues.CreateComment(ctx, owner, repo, number,
		&gogithub.IssueComment{Body: gogithub.Ptr(text)})
	if err != nil {
		return fmt.Errorf("github: create comment on %s: %w", channelID, err)
	}
	return nil
}
```

(If the fetched version predates `github.Ptr`, use `github.String(text)`.)

- [ ] **Step 4: Run tests**

```bash
go test -race ./internal/github/
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/github/send.go internal/github/send_test.go internal/github/github.go
git commit -m "feat(github): send replies as issue comments"
```

---

### Task 6: server.go — webhook handler (verify, filter, ack, dispatch)

**Files:**
- Create: `internal/github/server.go`
- Test: `internal/github/server_test.go`

**Interfaces:**
- Produces: `func (a *adapter) handleWebhook(dispatchCtx context.Context, w http.ResponseWriter, r *http.Request, deps core.AdapterDeps)` — used by Connect (Task 7). Increments/decrements `a.inflight` around dispatch.
- Consumes: `toMessage` (Task 4), `a.selfID` (Task 3 field), `a.inflight`.

- [ ] **Step 1: Write the failing tests** — `internal/github/server_test.go`:

```go
package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

const (
	prCommentCreated = `{
  "action": "created",
  "issue": {"number": 7, "pull_request": {"url": "https://api.github.com/repos/lao/botbooter/pulls/7"}},
  "comment": {"id": 2, "body": "/retest", "created_at": "2026-07-03T11:00:00Z",
    "user": {"id": 99, "login": "reviewer", "type": "User"}},
  "repository": {"full_name": "lao/botbooter"},
  "sender": {"id": 99, "login": "reviewer", "type": "User"}
}`
	commentEdited = `{
  "action": "edited",
  "issue": {"number": 42},
  "comment": {"id": 3, "body": "edited", "user": {"id": 99, "login": "reviewer", "type": "User"}},
  "repository": {"full_name": "lao/botbooter"}
}`
	botAuthoredComment = `{
  "action": "created",
  "issue": {"number": 42},
  "comment": {"id": 4, "body": "I am an app", "user": {"id": 555, "login": "some-app[bot]", "type": "Bot"}},
  "repository": {"full_name": "lao/botbooter"}
}`
	// PAT-shape self comment: type User, id matching the adapter's selfID.
	selfAuthoredComment = `{
  "action": "created",
  "issue": {"number": 42},
  "comment": {"id": 5, "body": "my own reply", "user": {"id": 777, "login": "bot-account", "type": "User"}},
  "repository": {"full_name": "lao/botbooter"}
}`
	pingEvent = `{"zen": "Design for failure.", "hook_id": 1}`
)

// sign returns the X-Hub-Signature-256 header value for body under secret.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// webhookRequest builds a signed issue_comment POST for the handler tests.
func webhookRequest(secret, event, payload string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	r.Header.Set("X-GitHub-Event", event)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Hub-Signature-256", sign(secret, []byte(payload)))
	return r
}

func captureDeps(got *[]*core.Message, done chan struct{}) core.AdapterDeps {
	return core.AdapterDeps{Dispatch: func(_ context.Context, m *core.Message) {
		*got = append(*got, m)
		if done != nil {
			done <- struct{}{}
		}
	}}
}

func awaitDispatch(t *testing.T, done chan struct{}, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for dispatch")
		}
	}
}

func TestHandleWebhook_DispatchesComment(t *testing.T) {
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	var got []*core.Message
	done := make(chan struct{}, 1)
	w := httptest.NewRecorder()

	a.handleWebhook(context.Background(), w, webhookRequest("hook-secret", "issue_comment", issueCommentCreated), captureDeps(&got, done))
	awaitDispatch(t, done, 1)

	asserts.Equal(t, w.Code, http.StatusOK, "authentic request should be 200")
	asserts.Equal(t, len(got), 1, "one message dispatched")
	asserts.Equal(t, got[0].ChannelID, "lao/botbooter#42", "channel id")
	asserts.Equal(t, got[0].Content, "/deploy staging", "content")
	raw, ok := RawEvent(got[0])
	asserts.True(t, ok, "raw event present")
	asserts.False(t, raw.Event.GetIssue().IsPullRequest(), "plain issue comment")
}

func TestHandleWebhook_PRComment(t *testing.T) {
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	var got []*core.Message
	done := make(chan struct{}, 1)

	a.handleWebhook(context.Background(), httptest.NewRecorder(), webhookRequest("hook-secret", "issue_comment", prCommentCreated), captureDeps(&got, done))
	awaitDispatch(t, done, 1)

	asserts.Equal(t, got[0].ChannelID, "lao/botbooter#7", "PR channel id")
	raw, _ := RawEvent(got[0])
	asserts.True(t, raw.Event.GetIssue().IsPullRequest(), "PR comment detectable via raw event")
}

// The handler must dispatch on exactly the detached context passed in, not the
// request context — otherwise a reply would fail with "context canceled"
// mid-drain (same guard as the WhatsApp adapter).
func TestHandleWebhook_DispatchesOnDetachedCtx(t *testing.T) {
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	dispatchCtx, cancelDispatch := context.WithCancel(context.Background())
	defer cancelDispatch()
	gotCtx := make(chan context.Context, 1)
	deps := core.AdapterDeps{Dispatch: func(c context.Context, _ *core.Message) { gotCtx <- c }}

	a.handleWebhook(dispatchCtx, httptest.NewRecorder(), webhookRequest("hook-secret", "issue_comment", issueCommentCreated), deps)

	select {
	case c := <-gotCtx:
		asserts.NoError(t, c.Err(), "dispatch ctx live before cancel")
		cancelDispatch()
		asserts.ErrorIs(t, c.Err(), context.Canceled, "dispatch rides the passed detached ctx")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch")
	}
}

func TestHandleWebhook_Rejections(t *testing.T) {
	cases := []struct {
		name     string
		request  func() *http.Request
		wantCode int
	}{
		{"BadSignature", func() *http.Request {
			r := webhookRequest("hook-secret", "issue_comment", issueCommentCreated)
			r.Header.Set("X-Hub-Signature-256", sign("wrong-secret", []byte(issueCommentCreated)))
			return r
		}, http.StatusForbidden},
		{"MissingSignature", func() *http.Request {
			r := webhookRequest("hook-secret", "issue_comment", issueCommentCreated)
			r.Header.Del("X-Hub-Signature-256")
			return r
		}, http.StatusForbidden},
		{"OversizedBody", func() *http.Request {
			big := strings.Repeat("a", maxRequestBytes+1)
			r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(big))
			r.Header.Set("X-Hub-Signature-256", sign("hook-secret", []byte(big)))
			return r
		}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := newAdapter(patConfig())
			asserts.NoError(t, err, "new adapter")
			var got []*core.Message
			w := httptest.NewRecorder()

			a.handleWebhook(context.Background(), w, tc.request(), captureDeps(&got, nil))

			asserts.Equal(t, w.Code, tc.wantCode, "status")
			asserts.Equal(t, len(got), 0, "nothing dispatched")
		})
	}
}

func TestHandleWebhook_AckedButDropped(t *testing.T) {
	cases := []struct {
		name    string
		event   string
		payload string
		prep    func(a *adapter)
	}{
		{"PingEvent", "ping", pingEvent, nil},
		{"EditedAction", "issue_comment", commentEdited, nil},
		{"BotAuthor", "issue_comment", botAuthoredComment, nil},
		{"SelfAuthor", "issue_comment", selfAuthoredComment, func(a *adapter) {
			a.mu.Lock()
			a.selfID, a.selfLogin = 777, "bot-account"
			a.mu.Unlock()
		}},
		{"UnknownEvent", "workflow_run", `{"action": "completed"}`, nil},
		{"MalformedJSON", "issue_comment", `not json{`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := newAdapter(patConfig())
			asserts.NoError(t, err, "new adapter")
			if tc.prep != nil {
				tc.prep(a)
			}
			var got []*core.Message
			w := httptest.NewRecorder()

			a.handleWebhook(context.Background(), w, webhookRequest("hook-secret", tc.event, tc.payload), captureDeps(&got, nil))

			asserts.Equal(t, w.Code, http.StatusOK, "dropped events still ack 200")
			// Dispatch is async; a dropped event never increments inflight, so
			// give a stray dispatch a moment to appear before asserting.
			time.Sleep(20 * time.Millisecond)
			asserts.Equal(t, len(got), 0, "nothing dispatched")
		})
	}
}

```

- [ ] **Step 2: Run to verify failure**

```bash
go test -race -run TestHandleWebhook ./internal/github/
```

Expected: FAIL — `undefined: a.handleWebhook`.

- [ ] **Step 3: Implement** — `internal/github/server.go` (handler only; Connect/Disconnect come in Task 7):

```go
package github

import (
	"context"
	"io"
	"log"
	"net/http"

	gogithub "github.com/google/go-github/v88/github"
	"github.com/lao/botbooter/internal/core"
)

const (
	signatureHeader = "X-Hub-Signature-256"
	eventHeader     = "X-GitHub-Event"
)

// handleWebhook authenticates, filters, acks and dispatches one webhook
// delivery. The ack (200) is written before dispatch runs: GitHub times out
// slow deliveries and disables hooks that fail persistently, so dropped and
// invalid-but-authentic payloads are acked too.
func (a *adapter) handleWebhook(dispatchCtx context.Context, w http.ResponseWriter, r *http.Request, deps core.AdapterDeps) {
	// Read then verify as two steps with two distinct failure codes (the
	// sibling-adapter pattern): a body we cannot read is the client's 400; a
	// body that fails HMAC is a 403. The one-shot ValidatePayload cannot
	// distinguish the two.
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := gogithub.ValidateSignature(r.Header.Get(signatureHeader), payload, []byte(a.cfg.WebhookSecret)); err != nil {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if gogithub.WebHookType(r) != "issue_comment" {
		w.WriteHeader(http.StatusOK) // ping and other subscribed events are not errors
		return
	}
	parsed, err := gogithub.ParseWebHook("issue_comment", payload)
	if err != nil {
		log.Printf("github: discarding webhook with unparseable body: %v", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	event, ok := parsed.(*gogithub.IssueCommentEvent)
	if !ok || event.GetAction() != "created" || a.isSelfOrBot(event) {
		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusOK)
	// Dispatch on the detached context: core cancels runCtx *before*
	// Disconnect's drain waits for this handler, so a reply threaded onto
	// runCtx would fail mid-drain. The increment lands before Shutdown
	// returns, so drainDispatch always observes it.
	a.inflight.Add(1)
	go func() {
		defer a.inflight.Add(-1)
		deps.Dispatch(dispatchCtx, toMessage(event))
	}()
}

// isSelfOrBot reports whether the comment author is any GitHub App bot (covers
// this bot in App mode and silences other bots wholesale, like the Slack and
// Discord adapters) or this bot's own account (the check that matters in PAT
// mode, where its comments arrive as a plain User).
func (a *adapter) isSelfOrBot(event *gogithub.IssueCommentEvent) bool {
	user := event.GetComment().GetUser()
	if user.GetType() == "Bot" {
		return true
	}
	a.mu.Lock()
	selfID := a.selfID
	a.mu.Unlock()
	return selfID != 0 && user.GetID() == selfID
}
```

- [ ] **Step 4: Run tests**

```bash
go test -race ./internal/github/
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/github/server.go internal/github/server_test.go
git commit -m "feat(github): webhook handler with signature verification and loop prevention"
```

---

### Task 7: server.go — Connect, self-identity, Disconnect + drain

**Files:**
- Modify: `internal/github/server.go` (add Connect/Disconnect/drainDispatch/resolveSelf)
- Modify: `internal/github/github.go` (delete Connect/Disconnect stubs; replace the `Attachments` stub with the real no-op)
- Test: `internal/github/server_test.go` (append lifecycle tests)

**Interfaces:**
- Produces: real `Connect(ctx, deps) error`, `Disconnect() error`, `Attachments(m) ([]core.Attachment, error)` (returns `nil, nil`). Adapter satisfies `core.Adapter` with no stubs left.
- Consumes: `handleWebhook` (Task 6), `testClient` helper (Task 5), fields from Task 3.

Self-identity endpoints the test server must fake:
- PAT mode: `GET /user` → `{"id": 777, "login": "bot-account"}`.
- App mode: `GET /app` (App-JWT client) → `{"slug": "my-app"}`, then `GET /users/my-app%5Bbot%5D` → `{"id": 555, "login": "my-app[bot]"}`.

For App-mode Connect tests the one-shot AppsTransport client must also hit the test server: give the adapter an unexported field `apiBaseURL string` set by `newAdapter` from a new **unexported-for-tests** hook — simplest is a field `baseURL string` on the adapter defaulting to `""` (production: whatever the client was built with) that tests set before Connect, and `resolveSelf` builds its one-shot client with `testClient`-style URLs when non-empty. Concretely:

```go
// on adapter struct (github.go):
	// baseURL overrides the API base for the one-shot self-identity client in
	// App mode; tests point it at an httptest server. Empty in production.
	baseURL string
```

PAT-mode self-identity reuses `a.client` directly (tests overwrite `a.client` with `testClient(t, srv)` before Connect, exactly like the Send tests).

- [ ] **Step 1: Write the failing tests** — append to `internal/github/server_test.go`:

```go
func selfIdentityServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"id": 777, "login": "bot-account"}`))
		case r.URL.Path == "/app":
			_, _ = w.Write([]byte(`{"id": 7, "slug": "my-app"}`))
		case strings.Contains(r.URL.Path, "/users/my-app"):
			_, _ = w.Write([]byte(`{"id": 555, "login": "my-app[bot]"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func connectedAdapter(t *testing.T, deps core.AdapterDeps) (*adapter, *httptest.Server, context.CancelFunc) {
	t.Helper()
	srv := selfIdentityServer(t)
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	asserts.NoError(t, a.Connect(ctx, deps), "connect")
	return a, srv, cancel
}

func TestConnect_ResolvesSelfAndBinds(t *testing.T) {
	a, srv, cancel := connectedAdapter(t, core.AdapterDeps{
		Dispatch:   func(context.Context, *core.Message) {},
		Done:       func(error) {},
		Disconnect: func() error { return nil },
	})
	defer srv.Close()
	defer cancel()
	defer func() { _ = a.Disconnect() }()

	a.mu.Lock()
	selfID, boundAddr := a.selfID, a.boundAddr
	a.mu.Unlock()
	asserts.Equal(t, selfID, int64(777), "PAT self-identity resolved via /user")
	asserts.True(t, boundAddr != "", "listener bound, addr recoverable")
}

func TestConnect_SelfIdentityFailureIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)

	err = a.Connect(context.Background(), core.AdapterDeps{})

	asserts.Error(t, err, "a bot that cannot recognize itself must not start")
	a.mu.Lock()
	defer a.mu.Unlock()
	asserts.True(t, a.srv == nil, "no server installed on failure")
}

func TestConnect_AppModeResolvesBotUser(t *testing.T) {
	srv := selfIdentityServer(t)
	defer srv.Close()
	a, err := newAdapter(appConfig(t))
	asserts.NoError(t, err, "new App adapter")
	a.client = testClient(t, srv)
	a.baseURL = srv.URL // one-shot App-JWT client also hits the fake

	asserts.NoError(t, a.Connect(context.Background(), core.AdapterDeps{
		Done: func(error) {}, Disconnect: func() error { return nil },
	}), "connect in App mode")
	defer func() { _ = a.Disconnect() }()

	a.mu.Lock()
	defer a.mu.Unlock()
	asserts.Equal(t, a.selfID, int64(555), "App self-identity resolved via /app then /users/{slug}[bot]")
}

func TestDisconnect_IdempotentAndClears(t *testing.T) {
	a, srv, cancel := connectedAdapter(t, core.AdapterDeps{
		Done: func(error) {}, Disconnect: func() error { return nil },
	})
	defer srv.Close()
	defer cancel()

	asserts.NoError(t, a.Disconnect(), "first disconnect")
	a.mu.Lock()
	cleared := a.srv == nil && a.boundAddr == "" && a.detachedCancel == nil
	stillSelf := a.selfID == 777
	a.mu.Unlock()
	asserts.True(t, cleared, "server state cleared")
	asserts.True(t, stillSelf, "self identity persists across Disconnect")
	asserts.NoError(t, a.Disconnect(), "second disconnect is a no-op")
}

func TestDisconnect_NeverConnected(t *testing.T) {
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	asserts.NoError(t, a.Disconnect(), "disconnect before connect is nil")
}

func TestConnect_CtxCancelTriggersDisconnect(t *testing.T) {
	disconnected := make(chan struct{}, 1)
	a, srv, cancel := connectedAdapter(t, core.AdapterDeps{
		Done:       func(error) {},
		Disconnect: func() error { disconnected <- struct{}{}; return nil },
	})
	defer srv.Close()
	defer func() { _ = a.Disconnect() }()

	cancel()

	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not call deps.Disconnect on ctx cancel")
	}
}

// A stale watcher from a superseded connection must not tear down its
// replacement: connect, disconnect, reconnect, then cancel the FIRST ctx and
// assert deps.Disconnect is not called for the second connection.
func TestConnect_StaleWatcherIgnoresReplacedServer(t *testing.T) {
	srv := selfIdentityServer(t)
	defer srv.Close()
	disconnects := make(chan struct{}, 2)
	deps := core.AdapterDeps{
		Done:       func(error) {},
		Disconnect: func() error { disconnects <- struct{}{}; return nil },
	}

	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	asserts.NoError(t, a.Connect(ctx1, deps), "first connect")
	asserts.NoError(t, a.Disconnect(), "disconnect first connection")

	asserts.NoError(t, a.Connect(context.Background(), deps), "reconnect")
	defer func() { _ = a.Disconnect() }()

	cancel1()

	select {
	case <-disconnects:
		t.Fatal("stale watcher tore down the replacement connection")
	case <-time.After(200 * time.Millisecond):
		// No Disconnect call: the stale watcher saw a.srv != its srv and bailed.
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	asserts.True(t, a.srv != nil, "second connection still installed")
}

// Disconnect must cancel the connection's detached dispatch context after the
// drain, so a stuck handler cannot leak past shutdown.
func TestDisconnect_CancelsDetachedCtx(t *testing.T) {
	gotCtx := make(chan context.Context, 1)
	a, srv, cancel := connectedAdapter(t, core.AdapterDeps{
		Dispatch:   func(c context.Context, _ *core.Message) { gotCtx <- c },
		Done:       func(error) {},
		Disconnect: func() error { return nil },
	})
	defer srv.Close()
	defer cancel()

	// Drive one real delivery through the bound server so dispatch runs on the
	// connection's detached context.
	a.mu.Lock()
	addr := a.boundAddr
	a.mu.Unlock()
	r, err := http.NewRequest(http.MethodPost, "http://"+addr+"/webhook", strings.NewReader(issueCommentCreated))
	asserts.NoError(t, err, "build request")
	r.Header.Set("X-GitHub-Event", "issue_comment")
	r.Header.Set("X-Hub-Signature-256", sign("hook-secret", []byte(issueCommentCreated)))
	resp, err := http.DefaultClient.Do(r)
	asserts.NoError(t, err, "deliver webhook")
	_ = resp.Body.Close()

	var dispatchCtx context.Context
	select {
	case dispatchCtx = <-gotCtx:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch")
	}
	asserts.NoError(t, dispatchCtx.Err(), "detached ctx live while connected")

	asserts.NoError(t, a.Disconnect(), "disconnect")
	asserts.ErrorIs(t, dispatchCtx.Err(), context.Canceled, "detached ctx canceled by Disconnect")
}

func TestAttachments_AlwaysNil(t *testing.T) {
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	atts, err := a.Attachments(&core.Message{})
	asserts.NoError(t, err, "no error")
	asserts.True(t, atts == nil, "v1 has no attachments")
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test -race -run 'TestConnect|TestDisconnect|TestAttachments' ./internal/github/
```

Expected: FAIL — stubs return nil without binding/resolving (`selfID` stays 0, `boundAddr` empty).

- [ ] **Step 3: Implement** — append to `internal/github/server.go`; delete the Connect/Disconnect/Attachments stubs in `github.go`, and add the `baseURL string` field to the adapter struct:

```go
func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	// One detached, cancelable context per connection parents all dispatch:
	// WithoutCancel lets an acked reply finish during the shutdown drain, and
	// WithCancel lets Disconnect abort stragglers after it.
	detachedCtx, detachedCancel := context.WithCancel(context.WithoutCancel(ctx))

	// A bot that cannot recognize itself is a reply-loop hazard: fail loudly
	// at startup, in either auth mode, rather than silently at dispatch.
	selfID, selfLogin, err := a.resolveSelf(ctx)
	if err != nil {
		detachedCancel()
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc(a.cfg.Path, func(w http.ResponseWriter, r *http.Request) {
		// GitHub webhooks are always POST; there is no GET handshake.
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		a.handleWebhook(detachedCtx, w, r, deps)
	})

	ln, err := net.Listen("tcp", a.cfg.Addr)
	if err != nil {
		detachedCancel()
		return err
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	a.mu.Lock()
	a.selfID, a.selfLogin = selfID, selfLogin
	a.srv = srv
	a.boundAddr = ln.Addr().String()
	a.detachedCancel = detachedCancel
	a.mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			deps.Done(err)
		}
	}()

	// Tear down when the run context is canceled; identity-compare so a stale
	// watcher from a superseded connection never tears down its replacement.
	go func() {
		<-ctx.Done()
		a.mu.Lock()
		current := a.srv == srv
		a.mu.Unlock()
		if current {
			_ = deps.Disconnect()
		}
	}()

	return nil
}

// resolveSelf resolves the bot's own account for loop prevention. PAT mode is
// one call; App mode cannot call GET /user with an installation token, so it
// asks GET /app (App JWT) for the slug, then resolves "<slug>[bot]" to an id.
func (a *adapter) resolveSelf(ctx context.Context) (int64, string, error) {
	if a.cfg.Token != "" {
		user, _, err := a.client.Users.Get(ctx, "")
		if err != nil {
			return 0, "", fmt.Errorf("github: resolve self identity: %w", err)
		}
		return user.GetID(), user.GetLogin(), nil
	}

	atr, err := ghinstallation.NewAppsTransport(a.baseTransport, a.cfg.AppID, a.cfg.PrivateKey)
	if err != nil {
		return 0, "", fmt.Errorf("github: build app transport: %w", err)
	}
	appClient, err := gogithub.NewClient(gogithub.WithHTTPClient(
		&http.Client{Transport: atr, Timeout: a.cfg.HTTPClient.Timeout},
	))
	if err != nil {
		return 0, "", fmt.Errorf("github: build app client: %w", err)
	}
	if a.baseURL != "" { // test hook: point the one-shot client at a fake API
		appClient, err = gogithub.NewClient(gogithub.WithHTTPClient(
			&http.Client{Transport: atr, Timeout: a.cfg.HTTPClient.Timeout},
		), gogithub.WithURLs(a.baseURL+"/", a.baseURL+"/"))
		if err != nil {
			return 0, "", fmt.Errorf("github: build app client: %w", err)
		}
	}

	app, _, err := appClient.Apps.Get(ctx, "")
	if err != nil {
		return 0, "", fmt.Errorf("github: resolve app slug: %w", err)
	}
	user, _, err := a.client.Users.Get(ctx, app.GetSlug()+"[bot]")
	if err != nil {
		return 0, "", fmt.Errorf("github: resolve bot user %s[bot]: %w", app.GetSlug(), err)
	}
	return user.GetID(), user.GetLogin(), nil
}

func (a *adapter) Disconnect() error {
	a.mu.Lock()
	srv := a.srv
	cancelDispatch := a.detachedCancel
	a.mu.Unlock()
	if srv == nil {
		return nil
	}
	// Shutdown and drain each get their own budget: dispatch goroutines run
	// outside the HTTP handler lifecycle, so a slow Shutdown must not consume
	// the drain deadline and drop an already-acked in-flight message.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutCancel()
	err := srv.Shutdown(shutCtx)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), drainTimeout)
	defer drainCancel()
	a.drainDispatch(drainCtx)

	var drainErr error
	if n := a.inflight.Load(); n > 0 {
		log.Printf("github: drain deadline reached; canceling %d in-flight dispatch(es)", n)
		drainErr = fmt.Errorf("github: dispatch drain timed out with %d in-flight dispatch(es)", n)
	}

	// Clear the shared fields only if a reconnect has not installed a newer
	// connection (identity-compare on srv). Either way, cancel THIS
	// connection's detached context after the drain so a stuck handler cannot
	// leak past shutdown. selfID/selfLogin persist: they are re-resolved on
	// the next Connect and harmless while no server is up.
	a.mu.Lock()
	if a.srv == srv {
		a.srv = nil
		a.boundAddr = ""
		a.detachedCancel = nil
	}
	a.mu.Unlock()

	if cancelDispatch != nil {
		cancelDispatch()
	}
	if err != nil {
		return err
	}
	return drainErr
}

// drainDispatch waits, bounded by ctx, for in-flight dispatch goroutines so an
// acked message is processed rather than dropped at shutdown. It polls an
// atomic counter rather than a WaitGroup: an Add racing Wait would risk a
// misuse panic.
func (a *adapter) drainDispatch(ctx context.Context) {
	for a.inflight.Load() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Attachments implements core.Adapter. GitHub issue comments carry markdown,
// not an upload channel worth modeling; v1 has no attachment support.
func (a *adapter) Attachments(_ *core.Message) ([]core.Attachment, error) {
	return nil, nil
}
```

Add the missing imports to server.go: `"errors"`, `"fmt"`, `"net"`, `"time"`, `"github.com/bradleyfalzon/ghinstallation/v2"`. Adjust `gogithub.NewClient`/`WithURLs` calls to the API variant recorded in Task 1 (classic variant: `appClient := gogithub.NewClient(&http.Client{...})`; test repoint via `appClient.WithEnterpriseURLs(a.baseURL, a.baseURL)`).

- [ ] **Step 4: Run tests**

```bash
go test -race ./internal/github/
```

Expected: PASS (including all earlier tasks' tests).

- [ ] **Step 5: Commit**

```bash
git add internal/github/
git commit -m "feat(github): webhook server lifecycle with self-identity and drain-safe shutdown"
```

---

### Task 8: Public facade package github/

**Files:**
- Create: `github/github.go`
- Create: `github/wrapper_test.go`
- Create: `github/imports_test.go`

**Interfaces:**
- Produces (consumer-facing API): `github.New(cfg) (*botbooter.Bot, error)`, `github.Client(b) *gogithub.Client`, `github.RawEvent(m) (*Message, bool)`, `github.Addr(b) string`, `Config`/`Message` aliases, `ErrMissingConfig`/`ErrAmbiguousAuth`/`ErrBadChannelID` sentinels.
- Consumes: everything exported from `internal/github` (Tasks 3–7).

- [ ] **Step 1: Write the failing tests** — `github/wrapper_test.go`:

```go
package github

import (
	"testing"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/internal/asserts"
)

func TestNew(t *testing.T) {
	bot, err := New(Config{Token: "ghp_x", WebhookSecret: "s", Addr: "127.0.0.1:0"})

	asserts.NoError(t, err, "new GitHub bot")
	asserts.Equal(t, bot.BotType, botbooter.GitHubBotType, "bot type")
}

func TestNewMissingConfig(t *testing.T) {
	_, err := New(Config{})

	asserts.ErrorIs(t, err, ErrMissingConfig, "missing config")
}

func TestNewAmbiguousAuth(t *testing.T) {
	_, err := New(Config{Token: "t", AppID: 1, InstallationID: 2, PrivateKey: []byte("k"),
		WebhookSecret: "s", Addr: ":0"})

	asserts.ErrorIs(t, err, ErrAmbiguousAuth, "both auth modes")
}

func TestRawEvent(t *testing.T) {
	want := &Message{}
	got, ok := RawEvent(&botbooter.Message{Raw: want})

	asserts.True(t, ok, "raw event present")
	asserts.Equal(t, got, want, "raw event")
}
```

and `github/imports_test.go`:

```go
package github

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestGitHubImportsNoForeignSDK locks in that the public GitHub wrapper imports
// no other platform's SDK directly — go-github and ghinstallation are its own.
// Direct-import guard only; the transitive build closure is proven by the
// module-level isolation deps test.
func TestGitHubImportsNoForeignSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"discordgo", "slack-go/slack", "go-telegram/bot"}, "github")
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test -race ./github/
```

Expected: FAIL — package `github/github.go` does not exist yet (no buildable Go files).

- [ ] **Step 3: Implement** — `github/github.go`:

```go
// Package github exposes the GitHub constructor, the raw-event accessor, and
// the Config/Message types for botbooter. Import it for a GitHub issue-ops bot:
// the adapter receives issue and PR comments over an issue_comment webhook and
// replies as issue comments through the GitHub REST API. A GitHub-only binary
// pulls in go-github and ghinstallation but never compiles discordgo, slack-go
// or go-telegram.
package github

import (
	gogithub "github.com/google/go-github/v88/github"
	"github.com/lao/botbooter"
	githubint "github.com/lao/botbooter/internal/github"
)

// Config configures a GitHub bot. Exactly one auth mode must be set: Token
// (PAT) or the AppID/InstallationID/PrivateKey triple (GitHub App).
type Config = githubint.Config

// Message is the typed raw payload of an inbound issue_comment event.
type Message = githubint.Message

// ErrMissingConfig is returned by [New] when a required [Config] field is empty.
var ErrMissingConfig = githubint.ErrMissingConfig

// ErrAmbiguousAuth is returned by [New] when both auth modes are configured.
var ErrAmbiguousAuth = githubint.ErrAmbiguousAuth

// ErrBadChannelID is returned by a GitHub bot's Send when the channel ID is not
// "owner/repo#number". Branch it with errors.Is.
var ErrBadChannelID = githubint.ErrBadChannelID

// New creates a GitHub bot. It runs an inbound webhook HTTP server at cfg.Addr,
// so put a TLS-terminating proxy in front and register the public HTTPS URL as
// the repository or App webhook (content type application/json, issue_comment
// events, with cfg.WebhookSecret). It returns [ErrMissingConfig] or
// [ErrAmbiguousAuth] on invalid config.
func New(cfg Config) (*botbooter.Bot, error) {
	return githubint.New(cfg)
}

// RawEvent returns the typed issue_comment event carried on m, reporting
// whether m originated from GitHub.
func RawEvent(m *botbooter.Message) (*Message, bool) {
	return githubint.RawEvent(m)
}

// Client returns the underlying go-github client, or nil if b is not a GitHub
// bot. Use it for API calls beyond the adapter's send path (labels, reactions,
// checks).
func Client(b *botbooter.Bot) *gogithub.Client {
	return githubint.Client(b)
}

// Addr returns the address b's webhook listener is bound to (host:port), or ""
// if b is not a GitHub bot or is not connected. Use it to recover the
// OS-assigned port after passing cfg.Addr ":0".
func Addr(b *botbooter.Bot) string {
	return githubint.Addr(b)
}
```

- [ ] **Step 4: Run tests**

```bash
go test -race ./github/
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add github/
git commit -m "feat(github): public facade package"
```

---

### Task 9: Isolation guard sweep

**Files:**
- Modify: `isolation_deps_test.go`
- Modify: `imports_guard_test.go`
- Modify: `cli/imports_test.go`, `slack/imports_test.go`, `discord/imports_test.go`, `telegram/imports_test.go`, `whatsapp/imports_test.go`, `teams/imports_test.go`

**Interfaces:**
- Consumes: the `github/` package path (Task 8). Uses the jwt major recorded in Task 1.

go-github is a platform SDK, so unlike jwt (crypto lib, excluded from direct-import bans) it joins **both** guard layers.

- [ ] **Step 1: Extend the transitive guard (write failing state first)** — in `isolation_deps_test.go`:

Add constants (version-agnostic substrings — go-github cuts majors often, and ghinstallation pins its *own* go-github major, so a versioned string would miss the second copy):

```go
		gogithubSDK = "github.com/google/go-github"
		ghinstall   = "github.com/bradleyfalzon/ghinstallation"
```

Change `allPlatforms` to:

```go
	allPlatforms := []string{"cli", "slack", "discord", "telegram", "whatsapp", "teams", "github"}
```

Append `gogithubSDK, ghinstall` to the `absent` slice of **every existing case row** (root, cli, slack, discord, telegram, whatsapp, teams), and add the new row (jwt stays absent-checked per-row as before; the github row *includes* jwt in `present` because ghinstallation pulls it — use the actual major from Task 1's `go mod graph`, likely `jwtv5`):

```go
		{"github.com/lao/botbooter/github", []string{discordgo, slackgo, gotelegram}, []string{gogithubSDK, ghinstall, jwtv5}, "github"},
```

If `go mod graph` showed ghinstallation pulling a jwt major other than v5, ban/expect that exact module path instead and leave a one-line comment.

- [ ] **Step 2: Extend the direct-import guards** — append `"google/go-github"` to the banned list in each of the six sibling `imports_test.go` files and the root `imports_guard_test.go`. Example (`teams/imports_test.go`):

```go
	asserts.CheckBannedImports(t, ".",
		[]string{"discordgo", "slack-go/slack", "go-telegram/bot", "google/go-github"}, "teams")
```

Same one-line change in `cli/`, `slack/`, `discord/`, `telegram/`, `whatsapp/` and in `imports_guard_test.go` (label `"botbooter"`).

- [ ] **Step 3: Run the guards**

```bash
go test -race -run 'TestIsolationDeps|ImportsNo' ./...
```

Expected: PASS — the github row's closure contains go-github + ghinstallation + jwt and no foreign SDKs; every other closure excludes go-github/ghinstallation. If the github row fails on jwt, fix the expected module path per `go mod graph`.

- [ ] **Step 4: Full suite**

```bash
go test -race ./...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add isolation_deps_test.go imports_guard_test.go cli/ slack/ discord/ telegram/ whatsapp/ teams/
git commit -m "test: extend isolation guards to the GitHub adapter's SDKs"
```

---

### Task 10: Docs + final gate

**Files:**
- Modify: `CLAUDE.md` (platform enumerations)
- Modify: `botbooter.go` (package doc platform list)

- [ ] **Step 1: Update platform lists** — in `CLAUDE.md`: add GitHub to the "What this is" sentence (webhook adapter, issue_comment events, replies as issue comments; note go-github + ghinstallation as its SDKs), and add `github` to every `{cli,slack,discord,telegram,whatsapp,teams}` package enumeration. In `botbooter.go`'s package doc: add GitHub to the platform list and `botbooter/github` to the per-platform package list (amend the "WhatsApp and Teams … need none" parenthetical: GitHub pulls go-github + ghinstallation).

- [ ] **Step 2: Full gate**

```bash
make all
```

Expected: fmt, vet, lint, test-race all green. If golangci-lint reports stale "0 issues" after doc-comment edits, clear its cache first (`golangci-lint cache clean`) — the repo has seen the cache mask revive/exported failures.

- [ ] **Step 3: Commit**

```bash
git add CLAUDE.md botbooter.go
git commit -m "docs: add GitHub to the platform lists"
```

---

## Deferred (spec Open Questions — do NOT implement)

- Retry/backoff on comment-creation rate limits (v1 surfaces the error).
- `Config.CommandPrefix` pre-filter for busy repos.
- `X-GitHub-Delivery` dedupe, GitHub Enterprise URLs, attachments.
