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
