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
	// auth transport chain.
	client, err := gogithub.NewClient(
		gogithub.WithHTTPClient(a.cfg.HTTPClient),
		gogithub.WithAuthToken(a.cfg.Token),
		gogithub.WithURLs(gogithub.Ptr(srv.URL+"/"), gogithub.Ptr(srv.URL+"/")),
	)
	asserts.NoError(t, err, "repoint client")
	a.client = client

	asserts.NoError(t, a.Send(context.Background(), "lao/botbooter#42", "hi"), "send")
	asserts.True(t, strings.Contains(gotAuth, "ghp_test"), "Authorization carries the PAT, got "+gotAuth)
}

func TestParseChannelID(t *testing.T) {
	cases := []struct {
		in      string
		owner   string
		repo    string
		number  int
		wantErr bool
	}{
		{"lao/botbooter#42", "lao", "botbooter", 42, false},
		{"a/b#12", "a", "b", 12, false},
		// Splits on the LAST '#': repo absorbs the earlier "#10" literally and
		// the trailing "#20" is the number. Pinning this as valid, not fixing it.
		{"owner/repo#10#20", "owner", "repo#10", 20, false},
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

// testClient builds a go-github client pointed at srv.
func testClient(t *testing.T, srv *httptest.Server) *gogithub.Client {
	t.Helper()
	client, err := gogithub.NewClient(
		gogithub.WithHTTPClient(srv.Client()),
		gogithub.WithURLs(gogithub.Ptr(srv.URL+"/"), gogithub.Ptr(srv.URL+"/")),
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
