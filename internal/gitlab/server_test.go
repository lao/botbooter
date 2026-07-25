package gitlab

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gogitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

const (
	systemNote = `{
  "object_kind": "note",
  "user": {"id": 58, "username": "octocat"},
  "project": {"path_with_namespace": "acme/widgets"},
  "object_attributes": {"id": 10, "note": "changed the title", "noteable_type": "Issue",
    "author_id": 58, "system": true, "action": "create"},
  "issue": {"iid": 17}
}`
	editedNote = `{
  "object_kind": "note",
  "user": {"id": 58, "username": "octocat"},
  "project": {"path_with_namespace": "acme/widgets"},
  "object_attributes": {"id": 11, "note": "edited", "noteable_type": "Issue",
    "author_id": 58, "system": false, "action": "update"},
  "issue": {"iid": 17}
}`
	// Self note: author_id matching the adapter's resolved selfID.
	selfNote = `{
  "object_kind": "note",
  "user": {"id": 777, "username": "bot-account"},
  "project": {"path_with_namespace": "acme/widgets"},
  "object_attributes": {"id": 12, "note": "my own reply", "noteable_type": "Issue",
    "author_id": 777, "system": false, "action": "create"},
  "issue": {"iid": 17}
}`
	// Commit comment: a valid note, but on a noteable with no reply target in v1.
	commitNote = `{
  "object_kind": "note",
  "user": {"id": 58, "username": "octocat"},
  "project": {"path_with_namespace": "acme/widgets"},
  "object_attributes": {"id": 13, "note": "nice", "noteable_type": "Commit",
    "author_id": 58, "system": false, "action": "create"},
  "commit": {"id": "abc123"}
}`

	mergeOpened = `{
  "object_kind": "merge_request",
  "user": {"id": 58, "username": "octocat"},
  "project": {"path_with_namespace": "acme/widgets"},
  "object_attributes": {"iid": 9, "action": "open", "author_id": 58, "title": "Add feature",
    "source_branch": "feature", "target_branch": "main", "state": "opened",
    "last_commit": {"id": "abc123"}}
}`
	mergeMerged = `{
  "object_kind": "merge_request",
  "user": {"id": 58, "username": "octocat"},
  "project": {"path_with_namespace": "acme/widgets"},
  "object_attributes": {"iid": 9, "action": "merge", "author_id": 58, "state": "merged"}
}`
	// Self-authored MR: author_id matching the adapter's resolved selfID.
	mergeSelf = `{
  "object_kind": "merge_request",
  "user": {"id": 777, "username": "bot-account"},
  "project": {"path_with_namespace": "acme/widgets"},
  "object_attributes": {"iid": 10, "action": "open", "author_id": 777, "state": "opened"}
}`
	pushToMain = `{
  "object_kind": "push",
  "ref": "refs/heads/main",
  "before": "abc123", "after": "def456",
  "user_id": 58, "user_username": "octocat",
  "project": {"path_with_namespace": "acme/widgets"}
}`
	// A comment on a confidential issue: GitLab delivers it on the Confidential
	// Note Hook trigger with issue.confidential set, and the reply inherits the
	// issue's restricted audience, so it dispatches.
	confidentialIssueNote = `{
  "object_kind": "note",
  "event_type": "confidential_note",
  "user": {"id": 58, "username": "octocat"},
  "project": {"path_with_namespace": "acme/widgets"},
  "object_attributes": {"id": 30, "note": "/deploy staging", "noteable_type": "Issue",
    "author_id": 58, "system": false, "action": "create"},
  "issue": {"iid": 17, "confidential": true}
}`
	// An *internal* note on a confidential issue: same trigger, same
	// issue.confidential, so only the note's own internal flag tells it apart from
	// the regular comment above.
	internalNoteOnConfidentialIssue = `{
  "object_kind": "note",
  "event_type": "confidential_note",
  "user": {"id": 58, "username": "octocat"},
  "project": {"path_with_namespace": "acme/widgets"},
  "object_attributes": {"id": 31, "note": "/deploy staging", "noteable_type": "Issue",
    "author_id": 58, "system": false, "action": "create", "internal": true},
  "issue": {"iid": 17, "confidential": true}
}`
	// The same internal note on the same confidential issue, from an instance that
	// predates the notes column's confidential -> internal rename and so sends the
	// flag under the old key.
	preRenameInternalNoteOnConfidentialIssue = `{
  "object_kind": "note",
  "event_type": "confidential_note",
  "user": {"id": 58, "username": "octocat"},
  "project": {"path_with_namespace": "acme/widgets"},
  "object_attributes": {"id": 31, "note": "/deploy staging", "noteable_type": "Issue",
    "author_id": 58, "system": false, "action": "create", "confidential": true},
  "issue": {"iid": 17, "confidential": true}
}`
	// An internal note flagged on a merge request, where the noteable can never be
	// confidential.
	internalNoteOnMerge = `{
  "object_kind": "note",
  "event_type": "confidential_note",
  "user": {"id": 58, "username": "octocat"},
  "project": {"path_with_namespace": "acme/widgets"},
  "object_attributes": {"id": 32, "note": "/deploy", "noteable_type": "MergeRequest",
    "author_id": 58, "system": false, "action": "create", "internal": true},
  "merge_request": {"iid": 9}
}`
	pingEvent = `{"event_name": "push"}`
	// Pre-16.11 GitLab (and older self-hosted) omit object_attributes.action on
	// note deliveries entirely; an absent action must be treated as a create.
	issueNoteNoAction = `{
  "object_kind": "note",
  "user": {"id": 58, "username": "octocat"},
  "project": {"path_with_namespace": "acme/widgets"},
  "object_attributes": {"id": 20, "note": "/deploy", "noteable_type": "Issue",
    "author_id": 58, "system": false},
  "issue": {"iid": 17}
}`
)

// failingListener makes Serve return err immediately, exercising the serve error
// filter without a real socket (mirrors the sibling adapters' tests).
type failingListener struct {
	err error
}

func (l failingListener) Accept() (net.Conn, error) { return nil, l.err }
func (failingListener) Close() error                { return nil }
func (failingListener) Addr() net.Addr              { return testAddr("failing") }

type testAddr string

func (testAddr) Network() string  { return "test" }
func (a testAddr) String() string { return string(a) }

// webhookRequest builds an authenticated GitLab webhook POST for the handler
// tests. GitLab authenticates on the X-Gitlab-Token header, not a body HMAC.
func webhookRequest(token, event, payload string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	r.Header.Set("X-Gitlab-Event", event)
	r.Header.Set("X-Gitlab-Token", token)
	r.Header.Set("Content-Type", "application/json")
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

func TestHandleWebhook_DispatchesIssueComment(t *testing.T) {
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	var got []*core.Message
	done := make(chan struct{}, 1)
	w := httptest.NewRecorder()

	a.handleWebhook(context.Background(), w, webhookRequest("hook-secret", "Note Hook", issueNoteCreated), captureDeps(&got, done))
	awaitDispatch(t, done, 1)

	asserts.Equal(t, w.Code, http.StatusOK, "authentic request should be 200")
	asserts.Equal(t, len(got), 1, "one message dispatched")
	asserts.Equal(t, got[0].ChannelID, "acme/widgets#17", "issue channel id")
	asserts.Equal(t, got[0].Content, "/deploy staging", "content")
	raw, ok := RawEvent(got[0])
	asserts.True(t, ok, "raw event present")
	asserts.True(t, raw.IssueComment != nil, "issue-comment raw set")
}

// A comment on a confidential issue arrives under the separate "Confidential
// Note Hook" trigger but is shaped identically, so it must dispatch like a plain
// note rather than being dropped as an unknown event.
func TestHandleWebhook_DispatchesConfidentialNote(t *testing.T) {
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	var got []*core.Message
	done := make(chan struct{}, 1)

	a.handleWebhook(context.Background(), httptest.NewRecorder(), webhookRequest("hook-secret", "Confidential Note Hook", confidentialIssueNote), captureDeps(&got, done))
	awaitDispatch(t, done, 1)

	asserts.Equal(t, len(got), 1, "confidential note dispatched")
	asserts.Equal(t, got[0].ChannelID, "acme/widgets#17", "issue channel id")
}

// The Confidential Note Hook also fires for an *internal* note on a visible
// issue or merge request. Send can only create a plain note, so answering one
// would publish the internal thread: those deliveries are acked and dropped.
// These fixtures carry no object_attributes.internal flag (an older instance, or
// one that omits it), so they exercise the noteable fallback — a
// non-confidential issue, or any merge request, which cannot be confidential.
func TestHandleWebhook_DropsInternalNoteOnVisibleNoteable(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"Issue", issueNoteCreated},
		{"MergeRequest", mergeNoteCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := newAdapter(testConfig())
			asserts.NoError(t, err, "new adapter")
			var got []*core.Message
			w := httptest.NewRecorder()

			a.handleWebhook(context.Background(), w, webhookRequest("hook-secret", "Confidential Note Hook", tc.payload), captureDeps(&got, nil))

			asserts.Equal(t, w.Code, http.StatusOK, "an internal note is acked, not rejected")
			// Dispatch is async; give a stray one a moment to appear.
			time.Sleep(20 * time.Millisecond)
			asserts.Equal(t, len(got), 0, "internal note on a visible noteable is dropped")
		})
	}
}

// An internal note on a *confidential* issue is the delivery the noteable cannot
// discriminate: it carries the same trigger and the same issue.confidential as a
// regular comment there. The note's own internal flag settles it, under either
// spelling, so a plain reply never publishes an internal thread to everyone who
// can see the issue.
func TestHandleWebhook_DropsFlaggedInternalNote(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"ConfidentialIssue", internalNoteOnConfidentialIssue},
		{"ConfidentialIssuePreRenameFlag", preRenameInternalNoteOnConfidentialIssue},
		{"MergeRequest", internalNoteOnMerge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := newAdapter(testConfig())
			asserts.NoError(t, err, "new adapter")
			var got []*core.Message
			w := httptest.NewRecorder()

			a.handleWebhook(context.Background(), w, webhookRequest("hook-secret", "Confidential Note Hook", tc.payload), captureDeps(&got, nil))

			asserts.Equal(t, w.Code, http.StatusOK, "an internal note is acked, not rejected")
			// Dispatch is async; give a stray one a moment to appear.
			time.Sleep(20 * time.Millisecond)
			asserts.Equal(t, len(got), 0, "a flagged internal note is dropped")
		})
	}
}

// GitLab omitted object_attributes.action on note deliveries before 16.11, so an
// absent action must be treated as a create — otherwise every comment on an
// older self-hosted instance is silently dropped. An explicit "update" (an edit)
// is still dropped; see TestHandleWebhook_AckedButDropped/EditedNote.
func TestHandleWebhook_AbsentActionDispatches(t *testing.T) {
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	var got []*core.Message
	done := make(chan struct{}, 1)

	a.handleWebhook(context.Background(), httptest.NewRecorder(), webhookRequest("hook-secret", "Note Hook", issueNoteNoAction), captureDeps(&got, done))
	awaitDispatch(t, done, 1)

	asserts.Equal(t, len(got), 1, "note with absent action dispatched as a create")
	asserts.Equal(t, got[0].Content, "/deploy", "content")
}

func TestHandleWebhook_DispatchesMergeComment(t *testing.T) {
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	var got []*core.Message
	done := make(chan struct{}, 1)

	a.handleWebhook(context.Background(), httptest.NewRecorder(), webhookRequest("hook-secret", "Note Hook", mergeNoteCreated), captureDeps(&got, done))
	awaitDispatch(t, done, 1)

	asserts.Equal(t, got[0].ChannelID, "acme/widgets!1", "MR channel id")
	raw, _ := RawEvent(got[0])
	asserts.True(t, raw.MergeComment != nil, "merge-comment raw set")
}

// The handler must dispatch on exactly the detached context passed in, not the
// request context — otherwise a reply would fail with "context canceled"
// mid-drain (same guard as the sibling adapters).
func TestHandleWebhook_DispatchesOnDetachedCtx(t *testing.T) {
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	dispatchCtx, cancelDispatch := context.WithCancel(context.Background())
	defer cancelDispatch()
	gotCtx := make(chan context.Context, 1)
	deps := core.AdapterDeps{Dispatch: func(c context.Context, _ *core.Message) { gotCtx <- c }}

	a.handleWebhook(dispatchCtx, httptest.NewRecorder(), webhookRequest("hook-secret", "Note Hook", issueNoteCreated), deps)

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
		{"BadToken", func() *http.Request {
			return webhookRequest("wrong-secret", "Note Hook", issueNoteCreated)
		}, http.StatusUnauthorized},
		{"MissingToken", func() *http.Request {
			r := webhookRequest("hook-secret", "Note Hook", issueNoteCreated)
			r.Header.Del("X-Gitlab-Token")
			return r
		}, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := newAdapter(testConfig())
			asserts.NoError(t, err, "new adapter")
			var got []*core.Message
			w := httptest.NewRecorder()

			a.handleWebhook(context.Background(), w, tc.request(), captureDeps(&got, nil))

			asserts.Equal(t, w.Code, tc.wantCode, "status")
			asserts.Equal(t, len(got), 0, "nothing dispatched")
		})
	}
}

// TestHandleWebhook_Routes401ToInjectedLogger proves the logger stored at
// Connect carries the token-rejection diagnostic, mirroring the Teams sibling.
func TestHandleWebhook_Routes401ToInjectedLogger(t *testing.T) {
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	var buf bytes.Buffer
	a.logger = slog.New(slog.NewTextHandler(&buf, nil))
	var got []*core.Message
	w := httptest.NewRecorder()

	a.handleWebhook(context.Background(), w, webhookRequest("wrong-secret", "Note Hook", issueNoteCreated), captureDeps(&got, nil))

	asserts.Equal(t, w.Code, http.StatusUnauthorized, "bad token should be 401")
	asserts.True(t, strings.Contains(buf.String(), "rejected with 401"),
		"the injected logger receives the rejection diagnostic")
	asserts.Equal(t, len(got), 0, "nothing dispatched")
}

// A saturated dispatch semaphore must ack 200 and drop instead of spawning an
// unbounded goroutine. It must NOT shed with 503: GitLab does not re-deliver a
// failed webhook, so the delivery is lost either way, and the failure would count
// toward auto-disabling the hook and suppress later deliveries too.
func TestHandleWebhook_DispatchSaturationAcksAndDrops(t *testing.T) {
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	a.sem = make(chan struct{}, 1) // force a one-slot bound
	var buf bytes.Buffer
	a.logger = slog.New(slog.NewTextHandler(&buf, nil))

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	deps := core.AdapterDeps{Dispatch: func(context.Context, *core.Message) {
		entered <- struct{}{}
		<-release
	}}

	w1 := httptest.NewRecorder()
	a.handleWebhook(context.Background(), w1, webhookRequest("hook-secret", "Note Hook", issueNoteCreated), deps)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first dispatch never entered")
	}
	asserts.Equal(t, w1.Code, http.StatusOK, "first delivery acquires a slot and is acked")

	w2 := httptest.NewRecorder()
	a.handleWebhook(context.Background(), w2, webhookRequest("hook-secret", "Note Hook", issueNoteCreated), deps)
	asserts.Equal(t, w2.Code, http.StatusOK, "saturated dispatch acks 200 rather than shedding")
	asserts.True(t, strings.Contains(buf.String(), "dispatch concurrency limit reached"),
		"the drop is logged, since the ack hides it from GitLab")

	close(release)
	drainCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a.drainDispatch(drainCtx)
	asserts.Equal(t, a.inflight.Load(), int64(0), "slot released after the handler returns")
	asserts.Equal(t, len(entered), 0, "the shed delivery never dispatched")
}

// A saturated read semaphore must ack 200 and drop before the body is buffered,
// for the same reason as the dispatch bound.
func TestHandleWebhook_ReadSaturationAcksAndDrops(t *testing.T) {
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	a.readSem = make(chan struct{}, 1)
	a.readSem <- struct{}{} // occupy the only inbound slot
	var buf bytes.Buffer
	a.logger = slog.New(slog.NewTextHandler(&buf, nil))

	var got []*core.Message
	w := httptest.NewRecorder()
	a.handleWebhook(context.Background(), w, webhookRequest("hook-secret", "Note Hook", issueNoteCreated), captureDeps(&got, nil))

	asserts.Equal(t, w.Code, http.StatusOK, "saturated read gate acks 200 rather than shedding")
	asserts.Equal(t, len(got), 0, "nothing dispatched")
	asserts.True(t, strings.Contains(buf.String(), "inbound concurrency limit reached"), "the drop is logged")
}

// An unreadable body — over the cap, or a truncated delivery — is acked 200 and
// logged, not answered 400: GitLab counts a 4xx toward auto-disabling the hook,
// so one oversized or truncated delivery would take the bot's ingress offline.
func TestHandleWebhook_OversizedBodyAcksAndDrops(t *testing.T) {
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	var buf bytes.Buffer
	a.logger = slog.New(slog.NewTextHandler(&buf, nil))

	// testConfig registers no large-event callback, so its cap is
	// smallRequestBytes; a body one byte over it trips MaxBytesReader.
	var got []*core.Message
	w := httptest.NewRecorder()
	big := strings.Repeat("a", smallRequestBytes+1)
	a.handleWebhook(context.Background(), w, webhookRequest("hook-secret", "Note Hook", big), captureDeps(&got, nil))

	asserts.Equal(t, w.Code, http.StatusOK, "an oversized body is acked, not rejected with 400")
	asserts.Equal(t, len(got), 0, "nothing dispatched")
	asserts.True(t, strings.Contains(buf.String(), "unreadable body"), "the drop is logged")
}

func TestHandleWebhook_AckedButDropped(t *testing.T) {
	cases := []struct {
		name    string
		event   string
		payload string
		prep    func(a *adapter)
	}{
		{"SystemNote", "Note Hook", systemNote, nil},
		{"EditedNote", "Note Hook", editedNote, nil},
		{"SelfNote", "Note Hook", selfNote, func(a *adapter) {
			a.mu.Lock()
			a.selfID = 777
			a.mu.Unlock()
		}},
		{"CommitNote", "Note Hook", commitNote, nil},
		{"UnknownEvent", "Pipeline Hook", `{"object_kind": "pipeline"}`, nil},
		{"MalformedNoteJSON", "Note Hook", `not json{`, nil},
		{"MergeNilCallback", "Merge Request Hook", mergeOpened, nil},
		{"PushNilCallback", "Push Hook", pushToMain, nil},
		{"PingEvent", "Push Hook", pingEvent, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := newAdapter(testConfig())
			asserts.NoError(t, err, "new adapter")
			if tc.prep != nil {
				tc.prep(a)
			}
			var got []*core.Message
			w := httptest.NewRecorder()

			a.handleWebhook(context.Background(), w, webhookRequest("hook-secret", tc.event, tc.payload), captureDeps(&got, nil))

			asserts.Equal(t, w.Code, http.StatusOK, "dropped events still ack 200")
			// Dispatch is async; a dropped event never increments inflight, so give
			// a stray dispatch a moment to appear before asserting.
			time.Sleep(20 * time.Millisecond)
			asserts.Equal(t, len(got), 0, "nothing dispatched")
		})
	}
}

// A Merge Request / Push delivery larger than the small cap but within the large
// one must be accepted and dispatched once a large-event callback is registered.
func TestHandleWebhook_LargePushWithinCap(t *testing.T) {
	cfg := testConfig()
	var got []*gogitlab.PushEvent
	done := make(chan struct{}, 1)
	cfg.OnPush = func(_ context.Context, event *gogitlab.PushEvent) {
		got = append(got, event)
		done <- struct{}{}
	}
	a, err := newAdapter(cfg)
	asserts.NoError(t, err, "new adapter")

	pad := strings.Repeat("a", smallRequestBytes+1) // one byte past the comment-path cap
	payload := `{"object_kind": "push", "ref": "refs/heads/main", "after": "def456",` +
		` "project": {"path_with_namespace": "acme/widgets"}, "_pad": "` + pad + `"}`
	asserts.True(t, len(payload) > smallRequestBytes, "payload exceeds the small cap")

	w := httptest.NewRecorder()
	a.handleWebhook(context.Background(), w, webhookRequest("hook-secret", "Push Hook", payload), core.AdapterDeps{})
	awaitDispatch(t, done, 1)

	asserts.Equal(t, w.Code, http.StatusOK, "large authentic push should be 200")
	asserts.Equal(t, len(got), 1, "large push dispatched, not dropped at the body cap")
	asserts.Equal(t, got[0].After, "def456", "full body parsed past the small cap")
}

func TestHandleWebhook_DispatchesMergeRequest(t *testing.T) {
	for _, action := range []string{"open", "reopen", "update"} {
		t.Run(action, func(t *testing.T) {
			payload := strings.Replace(mergeOpened, `"action": "open"`, `"action": "`+action+`"`, 1)
			cfg := testConfig()
			var got []*gogitlab.MergeEvent
			done := make(chan struct{}, 1)
			cfg.OnMergeRequest = func(_ context.Context, event *gogitlab.MergeEvent) {
				got = append(got, event)
				done <- struct{}{}
			}
			a, err := newAdapter(cfg)
			asserts.NoError(t, err, "new adapter")
			w := httptest.NewRecorder()

			a.handleWebhook(context.Background(), w, webhookRequest("hook-secret", "Merge Request Hook", payload), core.AdapterDeps{})
			awaitDispatch(t, done, 1)

			asserts.Equal(t, w.Code, http.StatusOK, "authentic request should be 200")
			asserts.Equal(t, len(got), 1, "one callback invocation")
			asserts.Equal(t, got[0].ObjectAttributes.Action, action, "action")
			asserts.Equal(t, got[0].ObjectAttributes.IID, int64(9), "MR iid")
		})
	}
}

func TestHandleWebhook_DispatchesPush(t *testing.T) {
	cfg := testConfig()
	var got []*gogitlab.PushEvent
	done := make(chan struct{}, 1)
	cfg.OnPush = func(_ context.Context, event *gogitlab.PushEvent) {
		got = append(got, event)
		done <- struct{}{}
	}
	a, err := newAdapter(cfg)
	asserts.NoError(t, err, "new adapter")
	w := httptest.NewRecorder()

	a.handleWebhook(context.Background(), w, webhookRequest("hook-secret", "Push Hook", pushToMain), core.AdapterDeps{})
	awaitDispatch(t, done, 1)

	asserts.Equal(t, w.Code, http.StatusOK, "authentic request should be 200")
	asserts.Equal(t, len(got), 1, "one callback invocation")
	asserts.Equal(t, got[0].Ref, "refs/heads/main", "ref")
	asserts.Equal(t, got[0].After, "def456", "after sha")
}

// The Merge Request path applies the same drops as the comment path even with a
// callback registered: non-reviewable actions, self authors, and unparseable
// payloads all ack 200 without invoking it.
func TestHandleWebhook_MergeRequestDroppedWithCallback(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		prep    func(a *adapter)
	}{
		{"MergedAction", mergeMerged, nil},
		{"SelfAuthor", mergeSelf, func(a *adapter) {
			a.mu.Lock()
			a.selfID = 777
			a.mu.Unlock()
		}},
		{"MalformedJSON", `not json{`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			var calls atomic.Int64
			cfg.OnMergeRequest = func(context.Context, *gogitlab.MergeEvent) { calls.Add(1) }
			a, err := newAdapter(cfg)
			asserts.NoError(t, err, "new adapter")
			if tc.prep != nil {
				tc.prep(a)
			}
			w := httptest.NewRecorder()

			a.handleWebhook(context.Background(), w, webhookRequest("hook-secret", "Merge Request Hook", tc.payload), core.AdapterDeps{})

			asserts.Equal(t, w.Code, http.StatusOK, "dropped events still ack 200")
			time.Sleep(20 * time.Millisecond)
			asserts.Equal(t, calls.Load(), int64(0), "callback not invoked")
		})
	}
}

// A running callback holds the inflight counter Disconnect's drain waits on.
func TestHandleWebhook_CallbackCoveredByDrain(t *testing.T) {
	cfg := testConfig()
	entered := make(chan struct{})
	release := make(chan struct{})
	cfg.OnMergeRequest = func(context.Context, *gogitlab.MergeEvent) {
		close(entered)
		<-release
	}
	a, err := newAdapter(cfg)
	asserts.NoError(t, err, "new adapter")
	w := httptest.NewRecorder()

	a.handleWebhook(context.Background(), w, webhookRequest("hook-secret", "Merge Request Hook", mergeOpened), core.AdapterDeps{})
	<-entered
	asserts.Equal(t, a.inflight.Load(), int64(1), "running callback holds the inflight counter")

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for a.inflight.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	asserts.Equal(t, a.inflight.Load(), int64(0), "callback return releases the counter")
}

func selfIdentityServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/user") {
			_, _ = w.Write([]byte(`{"id": 777, "username": "bot-account"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

// waitForSelfID polls until the serve goroutine has resolved the adapter's self
// identity; resolution is asynchronous by design (Connect is non-blocking).
func waitForSelfID(t *testing.T, a *adapter, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		id := a.selfID
		a.mu.Unlock()
		if id == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("self identity not resolved to %d in time", want)
}

func connectedAdapter(t *testing.T, deps core.AdapterDeps) (*adapter, *httptest.Server, context.CancelFunc) {
	t.Helper()
	srv := selfIdentityServer(t)
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	asserts.NoError(t, a.Connect(ctx, deps), "connect")
	return a, srv, cancel
}

// GitLab webhooks are always POST; anything else is rejected before the token
// check ever runs.
func TestConnect_NonPostMethodRejected(t *testing.T) {
	a, srv, cancel := connectedAdapter(t, core.AdapterDeps{
		Dispatch:   func(context.Context, *core.Message) {},
		Done:       func(error) {},
		Disconnect: func() error { return nil },
	})
	defer srv.Close()
	defer cancel()
	defer func() { _ = a.Disconnect() }()

	a.mu.Lock()
	addr := a.boundAddr
	a.mu.Unlock()
	resp, err := http.Get("http://" + addr + "/webhook")
	asserts.NoError(t, err, "GET webhook")
	_ = resp.Body.Close()
	asserts.Equal(t, resp.StatusCode, http.StatusMethodNotAllowed, "non-POST is 405")
}

func TestConnect_ListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	asserts.NoError(t, err, "occupy port")
	defer func() { _ = ln.Close() }()

	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	a.cfg.Addr = ln.Addr().String()

	err = a.Connect(context.Background(), core.AdapterDeps{})
	asserts.Error(t, err, "Connect must fail when the address is taken")
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
	boundAddr := a.boundAddr
	a.mu.Unlock()
	asserts.True(t, boundAddr != "", "listener bound, addr recoverable")
	waitForSelfID(t, a, 777) // resolved asynchronously via /user before serving
}

// A bot that cannot recognize itself is a reply-loop hazard: an identity failure
// must surface fatally via deps.Done (Connect itself stays non-blocking).
func TestConnect_SelfIdentityFailureIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)

	failed := make(chan error, 1)
	asserts.NoError(t, a.Connect(context.Background(), core.AdapterDeps{
		Done:       func(err error) { failed <- err },
		Disconnect: func() error { return nil },
	}), "Connect is non-blocking; the failure arrives via Done")
	defer func() { _ = a.Disconnect() }()

	select {
	case err := <-failed:
		asserts.Error(t, err, "a bot that cannot recognize itself must not serve")
	case <-time.After(2 * time.Second):
		t.Fatal("identity failure never surfaced via deps.Done")
	}

	// Serve never runs on this path, so the ln.Close in the serve goroutine is
	// the only thing that releases the listener — pin that it actually did.
	a.mu.Lock()
	addr := a.boundAddr
	a.mu.Unlock()
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("listener still accepting connections after identity failure")
	}
}

// Connect must return before the self-identity round-trip completes: core holds
// the bot lock across adapter.Connect and documents it as non-blocking.
func TestConnect_DoesNotBlockOnSelfResolution(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // hold /user until the test releases it
		_, _ = w.Write([]byte(`{"id": 777, "username": "bot-account"}`))
	}))
	defer srv.Close()
	defer unblock()
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)

	connected := make(chan error, 1)
	go func() {
		connected <- a.Connect(context.Background(), core.AdapterDeps{
			Done: func(error) {}, Disconnect: func() error { return nil },
		})
	}()
	select {
	case err := <-connected:
		asserts.NoError(t, err, "connect")
	case <-time.After(time.Second):
		t.Fatal("Connect blocked on the self-identity round-trip")
	}
	defer func() { _ = a.Disconnect() }()

	unblock()
	waitForSelfID(t, a, 777)
}

// A hung self-identity probe must time out on the adapter's own budget rather
// than wait on the HTTP client. Config.HTTPClient is a supported knob, and a
// consumer-supplied client need not set a Timeout; with a run context that has
// no deadline, an unreachable-but-non-rejecting API would otherwise hang the
// probe forever — listener bound, Serve never started, deps.Done never called,
// so Run blocks with no error while GitLab's deliveries time out in the backlog.
func TestConnect_SelfIdentityProbeTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // never answer /user within the probe's budget
		_, _ = w.Write([]byte(`{"id": 777, "username": "bot-account"}`))
	}))
	defer srv.Close()
	defer close(release)

	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	// testClient carries no Timeout of its own — the same shape as a
	// consumer-supplied Config.HTTPClient that omits one, so nothing but the
	// adapter's own budget can bound the probe.
	a.client = testClient(t, srv)
	// Set before Connect reads it; no concurrent access.
	a.selfResolveBudget = 50 * time.Millisecond

	failed := make(chan error, 1)
	// A run context with no deadline whose owner never calls Disconnect.
	asserts.NoError(t, a.Connect(context.Background(), core.AdapterDeps{
		Done:       func(err error) { failed <- err },
		Disconnect: func() error { return nil },
	}), "Connect is non-blocking; the timeout arrives via Done")
	defer func() { _ = a.Disconnect() }()

	select {
	case err := <-failed:
		asserts.Error(t, err, "a hung identity probe must surface via Done")
		asserts.ErrorIs(t, err, context.DeadlineExceeded, "the probe's own budget bounded it")
	case <-time.After(2 * time.Second):
		t.Fatal("hung identity probe never surfaced via deps.Done")
	}
}

// A non-default cfg.Path must actually route: the webhook lands on the custom
// route and the default route no longer exists.
func TestConnect_CustomPathRoutes(t *testing.T) {
	srv := selfIdentityServer(t)
	defer srv.Close()

	cfg := testConfig()
	cfg.Path = "/hooks/gl"
	a, err := newAdapter(cfg)
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)

	var got []*core.Message
	dispatched := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := captureDeps(&got, dispatched)
	deps.Done = func(error) {}
	deps.Disconnect = func() error { return nil }
	asserts.NoError(t, a.Connect(ctx, deps), "connect")
	defer func() { _ = a.Disconnect() }()

	a.mu.Lock()
	addr := a.boundAddr
	a.mu.Unlock()
	post := func(path string) int {
		r, err := http.NewRequest(http.MethodPost, "http://"+addr+path, strings.NewReader(issueNoteCreated))
		asserts.NoError(t, err, "build request")
		r.Header.Set("X-Gitlab-Event", "Note Hook")
		r.Header.Set("X-Gitlab-Token", "hook-secret")
		resp, err := http.DefaultClient.Do(r)
		asserts.NoError(t, err, "deliver webhook")
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	asserts.Equal(t, post("/hooks/gl"), http.StatusOK, "custom path serves the webhook")
	awaitDispatch(t, dispatched, 1)
	asserts.Equal(t, len(got), 1, "delivery on the custom path dispatched")
	asserts.Equal(t, post("/webhook"), http.StatusNotFound, "default path is not registered")
}

func TestDisconnect_IdempotentAndClears(t *testing.T) {
	// A clean Disconnect must never report through deps.Done: Serve's
	// ErrServerClosed is the expected result of Shutdown, and serve's filter is
	// what keeps it out of the done channel.
	a, srv, cancel := connectedAdapter(t, core.AdapterDeps{
		Done:       func(err error) { t.Errorf("deps.Done called on clean Disconnect: %v", err) },
		Disconnect: func() error { return nil },
	})
	defer srv.Close()
	defer cancel()

	waitForSelfID(t, a, 777) // settle async resolution before tearing down
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
	a, err := newAdapter(testConfig())
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
// replacement.
func TestConnect_StaleWatcherIgnoresReplacedServer(t *testing.T) {
	srv := selfIdentityServer(t)
	defer srv.Close()
	disconnects := make(chan struct{}, 2)
	deps := core.AdapterDeps{
		Done:       func(error) {},
		Disconnect: func() error { disconnects <- struct{}{}; return nil },
	}

	a, err := newAdapter(testConfig())
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

	a.mu.Lock()
	addr := a.boundAddr
	a.mu.Unlock()
	r, err := http.NewRequest(http.MethodPost, "http://"+addr+"/webhook", strings.NewReader(issueNoteCreated))
	asserts.NoError(t, err, "build request")
	r.Header.Set("X-Gitlab-Event", "Note Hook")
	r.Header.Set("X-Gitlab-Token", "hook-secret")
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

// Disconnect must cancel the detached context only AFTER the drain window: a
// dispatch still running when Disconnect starts must observe a live context
// through to completion.
func TestDisconnect_DrainsBeforeCancelingDetachedCtx(t *testing.T) {
	entered := make(chan struct{}, 1)
	ctxErrAtEnd := make(chan error, 1)
	a, srv, cancel := connectedAdapter(t, core.AdapterDeps{
		Dispatch: func(c context.Context, _ *core.Message) {
			entered <- struct{}{}
			time.Sleep(100 * time.Millisecond) // still in flight when Disconnect runs
			ctxErrAtEnd <- c.Err()
		},
		Done:       func(error) {},
		Disconnect: func() error { return nil },
	})
	defer srv.Close()
	defer cancel()

	a.mu.Lock()
	addr := a.boundAddr
	a.mu.Unlock()
	r, err := http.NewRequest(http.MethodPost, "http://"+addr+"/webhook", strings.NewReader(issueNoteCreated))
	asserts.NoError(t, err, "build request")
	r.Header.Set("X-Gitlab-Event", "Note Hook")
	r.Header.Set("X-Gitlab-Token", "hook-secret")
	resp, err := http.DefaultClient.Do(r)
	asserts.NoError(t, err, "deliver webhook")
	_ = resp.Body.Close()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch to start")
	}

	asserts.NoError(t, a.Disconnect(), "disconnect during in-flight dispatch")

	select {
	case dispatchErr := <-ctxErrAtEnd:
		asserts.NoError(t, dispatchErr, "in-flight dispatch kept a live detached ctx until it finished (drain before cancel)")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch to finish")
	}
}

func TestDrainDispatch_ContextCancel(t *testing.T) {
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	a.inflight.Add(1) // never decremented
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.drainDispatch(ctx) // must return promptly on a canceled ctx
	asserts.Equal(t, a.inflight.Load(), int64(1), "drain returns on ctx cancel without waiting")
}

func TestDrainDispatch_WaitsForInflight(t *testing.T) {
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	a.inflight.Add(1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		a.inflight.Add(-1)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a.drainDispatch(ctx)

	asserts.Equal(t, a.inflight.Load(), int64(0), "drain should wait until in-flight dispatch reaches zero")
}

func TestServe_ReportsUnexpectedError(t *testing.T) {
	want := errors.New("accept failed")
	done := make(chan error, 1)

	serve(&http.Server{}, failingListener{err: want}, func(err error) {
		done <- err
	})

	select {
	case got := <-done:
		asserts.ErrorIs(t, got, want, "unexpected serve error should be reported")
	default:
		t.Fatal("serve returned without reporting the listener error")
	}
}

func TestAttachments_AlwaysNil(t *testing.T) {
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	atts, err := a.Attachments(&core.Message{})
	asserts.NoError(t, err, "no error")
	asserts.True(t, atts == nil, "v1 has no attachments")
}

// TestDisconnect_DrainTimeoutReturnsError guards that a drain which cannot finish
// within its deadline surfaces an error. The production budget is drainTimeout
// (5s); the test shortens the adapter's injected drainBudget so the same path
// runs in milliseconds and stays in the default CI suite.
func TestDisconnect_DrainTimeoutReturnsError(t *testing.T) {
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	a.drainBudget = 50 * time.Millisecond // set before Disconnect reads it; no concurrent access
	// Unstarted server: Shutdown returns immediately, so only the drain can time out.
	a.mu.Lock()
	a.srv = &http.Server{}
	a.detachedCancel = func() {}
	a.mu.Unlock()

	release := make(chan struct{})
	defer close(release)
	dispatched := make(chan struct{}, 1)
	deps := core.AdapterDeps{Dispatch: func(context.Context, *core.Message) {
		dispatched <- struct{}{}
		<-release // block past the drain deadline
	}}

	a.handleWebhook(context.Background(), httptest.NewRecorder(), webhookRequest("hook-secret", "Note Hook", issueNoteCreated), deps)

	select {
	case <-dispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch to start")
	}

	asserts.Error(t, a.Disconnect(), "drain timeout should surface an error, not a clean nil")
}
