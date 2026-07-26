package gitlab

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gogitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// rewriteTransport reroutes every request to the test server without touching
// anything else, so a test can drive the adapter's own client — auth header and
// all — instead of rebuilding one and vouching for itself.
type rewriteTransport struct{ host string }

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = rt.host
	return http.DefaultTransport.RoundTrip(req)
}

// testClient builds a client-go client pointed at srv, with retries off so an
// error-path test does not stall on backoff.
func testClient(t *testing.T, srv *httptest.Server) *gogitlab.Client {
	t.Helper()
	client, err := gogitlab.NewClient("glpat-test",
		gogitlab.WithBaseURL(srv.URL),
		gogitlab.WithHTTPClient(srv.Client()),
		gogitlab.WithoutRetries())
	asserts.NoError(t, err, "build test client")
	return client
}

func TestSend_PostsIssueNote(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 1}`))
	}))
	defer srv.Close()

	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)

	asserts.NoError(t, a.Send(context.Background(), "acme/widgets#17", "done", core.SendOptions{}), "send")
	// The project path is URL-encoded (%2F) so a slash in it cannot escape its
	// path position; the issue iid routes to the issue notes endpoint.
	asserts.True(t, strings.HasSuffix(gotPath, "/projects/acme%2Fwidgets/issues/17/notes"), "issue note endpoint, got "+gotPath)
	var payload struct {
		Body string `json:"body"`
	}
	asserts.NoError(t, json.Unmarshal([]byte(gotBody), &payload), "request body is JSON")
	asserts.Equal(t, payload.Body, "done", "note body")
}

func TestSend_PostsMergeNote(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 1}`))
	}))
	defer srv.Close()

	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)

	asserts.NoError(t, a.Send(context.Background(), "acme/widgets!3", "ok", core.SendOptions{}), "send")
	asserts.True(t, strings.HasSuffix(gotPath, "/projects/acme%2Fwidgets/merge_requests/3/notes"), "merge-request note endpoint, got "+gotPath)
}

// TestSend_PrivateTokenHeader asserts the token wiring: the client built by
// newAdapter must send the access token on outbound calls as the Private-Token
// header. The adapter's own client is used unmodified (only the wire is
// redirected), so dropping the token from newAdapter fails this test.
func TestSend_PrivateTokenHeader(t *testing.T) {
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("Private-Token")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 1}`))
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.HTTPClient = &http.Client{Transport: rewriteTransport{host: srv.Listener.Addr().String()}}
	a, err := newAdapter(cfg)
	asserts.NoError(t, err, "new adapter")

	asserts.NoError(t, a.Send(context.Background(), "acme/widgets#17", "hi", core.SendOptions{}), "send")
	asserts.Equal(t, gotToken, "glpat-test", "Private-Token header carries the access token")
}

func TestParseChannelID(t *testing.T) {
	cases := []struct {
		in      string
		project string
		iid     int64
		kind    noteableKind
		wantErr bool
	}{
		{"acme/widgets#17", "acme/widgets", 17, noteableIssue, false},
		{"acme/widgets!3", "acme/widgets", 3, noteableMergeRequest, false},
		{"group/sub/proj!5", "group/sub/proj", 5, noteableMergeRequest, false}, // nested namespace
		// Splits on the FIRST separator: a second one lands in the iid part and
		// fails ParseInt, so a double-separator id is rejected up front.
		{"acme/widgets#1#2", "", 0, 0, true},
		{"acme/widgets!1!2", "", 0, 0, true},
		{"acme/widgets", "", 0, 0, true},   // no separator
		{"widgets#1", "", 0, 0, true},      // no namespace (<2 path segments)
		{"acme/widgets#0", "", 0, 0, true}, // non-positive iid
		{"acme/widgets#-1", "", 0, 0, true},
		{"acme/widgets#abc", "", 0, 0, true},
		{"acme/#1", "", 0, 0, true}, // empty project segment
		{"#1", "", 0, 0, true},      // empty project
		{"", "", 0, 0, true},
		// Charset/segment hardening: a segment must be a legal path segment
		// ([A-Za-z0-9._-]) and never "." or ".." — otherwise a crafted id could
		// walk or inject the REST path.
		{"../widgets#1", "", 0, 0, true},     // ".." namespace traverses the path
		{"acme/..#1", "", 0, 0, true},        // ".." project traverses the path
		{"acme/wid gets#1", "", 0, 0, true},  // space
		{"acme/wid:gets#1", "", 0, 0, true},  // colon
		{"acme%2Fwidgets#1", "", 0, 0, true}, // percent-encoded separator
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			project, iid, kind, err := parseChannelID(tc.in)
			if tc.wantErr {
				asserts.ErrorIs(t, err, ErrBadChannelID, "malformed id")
				return
			}
			asserts.NoError(t, err, "valid id")
			asserts.Equal(t, project, tc.project, "project")
			asserts.Equal(t, iid, tc.iid, "iid")
			asserts.Equal(t, kind, tc.kind, "kind")
		})
	}
}

func TestSend_BadChannelID(t *testing.T) {
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")

	asserts.ErrorIs(t, a.Send(context.Background(), "not-a-channel", "x", core.SendOptions{}), ErrBadChannelID, "malformed channel id")
}

func TestSend_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message": "403 Forbidden"}`))
	}))
	defer srv.Close()

	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)

	err = a.Send(context.Background(), "acme/widgets#17", "x", core.SendOptions{})
	asserts.Error(t, err, "API failure surfaces")
	asserts.True(t, strings.Contains(err.Error(), "acme/widgets#17"), "error names the channel")
}
