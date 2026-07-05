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
