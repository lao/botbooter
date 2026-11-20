package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lao/botbooter/github"
)

type githubTransport func(*http.Request) (*http.Response, error)

func (f githubTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPollWatcherWelcomesEachEligiblePROnce(t *testing.T) {
	t.Setenv("GITHUB_REPO", "ORG/*")
	t.Setenv("GITHUB_PRIVATE_KEY_FILE", "")
	t.Setenv("GITHUB_PR_POLL_SECONDS", "1")
	synctest.Test(t, func(t *testing.T) {
		comments := 0
		searches := 0
		transport := githubTransport(func(r *http.Request) (*http.Response, error) {
			if r.Body != nil {
				defer r.Body.Close()
			}
			var body string
			switch r.URL.Path {
			case "/user/repos":
				body = `[{"full_name":"org/repo","owner":{"login":"org","type":"Organization"}},{"full_name":"org/retired","archived":true,"owner":{"login":"org"}},{"full_name":"other/repo","owner":{"login":"other"}}]`
			case "/search/issues":
				searches++
				if !strings.Contains(r.URL.Query().Get("q"), "org:org") {
					t.Errorf("search query = %q", r.URL.RawQuery)
				}
				body = `{"items":[{"number":2,"repository_url":"https://api.github.com/repos/org/repo","user":{"login":"dev","type":"User"}},{"number":3,"repository_url":"https://api.github.com/repos/org/repo","user":{"type":"Bot"}},{"number":4,"repository_url":"https://api.github.com/repos/org/retired","user":{"type":"User"}}]}`
			case "/repos/org/repo/issues/2/comments":
				comments++
				text, err := io.ReadAll(r.Body)
				if err != nil || !strings.Contains(string(text), "@dev") {
					t.Errorf("welcome = %q, %v", text, err)
				}
				body = `{"id":1}`
			default:
				t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL)
				body = "{}"
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})
		bot, err := github.New(github.Config{Token: "test", WebhookSecret: "test", Addr: ":0", HTTPClient: &http.Client{Transport: transport}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		if err := startPollWatcher(ctx, bot); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2500 * time.Millisecond)
		cancel()
		synctest.Wait()
		if searches != 2 || comments != 1 {
			t.Fatalf("searches = %d, welcome comments = %d", searches, comments)
		}
	})
}

func TestGitHubModesConstructWithoutConnecting(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "token")
	t.Setenv("GITHUB_PRIVATE_KEY_FILE", "")
	t.Setenv("GITHUB_ADDR", "")
	t.Setenv("GITHUB_WEBHOOK_SECRET", "")
	poll, err := newBot(nil)
	if err != nil || github.Client(poll) == nil {
		t.Fatalf("poll bot = %v, %v", poll, err)
	}
	t.Setenv("GITHUB_WEBHOOK_SECRET", "secret")
	webhook, err := newWebhookBot()
	if err != nil || github.Client(webhook) == nil {
		t.Fatalf("webhook bot = %v, %v", webhook, err)
	}
}
