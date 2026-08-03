package bitbucket

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	gobitbucket "github.com/ktrysmt/go-bitbucket"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

func TestParseChannelID(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
		project string
		repo    string
		id      int64
		issue   bool
	}{
		{name: "CloudPR", in: "myws/myrepo!42", project: "myws", repo: "myrepo", id: 42, issue: false},
		{name: "CloudIssue", in: "myws/myrepo#7", project: "myws", repo: "myrepo", id: 7, issue: true},
		{name: "DCProjectKey", in: "PROJ/myrepo!42", project: "PROJ", repo: "myrepo", id: 42, issue: false},
		{name: "DCPersonalRepo", in: "~john/myrepo!5", project: "~john", repo: "myrepo", id: 5, issue: false},
		{name: "NoSigil", in: "myws/myrepo", wantErr: true},
		{name: "NonNumericID", in: "myws/myrepo!x", wantErr: true},
		{name: "ZeroID", in: "myws/myrepo!0", wantErr: true},
		{name: "NegativeID", in: "myws/myrepo!-3", wantErr: true},
		{name: "SingleSegment", in: "single!1", wantErr: true},
		{name: "ThreeSegments", in: "a/b/c!1", wantErr: true},
		{name: "DotDotSegment", in: "ws/..!1", wantErr: true},
		{name: "SlashInjection", in: "ws/re/po!1", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseChannelID(tc.in)
			if tc.wantErr {
				asserts.ErrorIs(t, err, ErrBadChannelID, "bad channel id")
				return
			}
			asserts.NoError(t, err, "parse")
			asserts.Equal(t, got.project, tc.project, "project")
			asserts.Equal(t, got.repo, tc.repo, "repo")
			asserts.Equal(t, got.id, tc.id, "id")
			asserts.Equal(t, got.issue, tc.issue, "issue")
		})
	}
}

// capture records the last request a test server received.
type capture struct {
	method string
	path   string
	body   map[string]any
}

func recordingServer(t *testing.T, status int) (*httptest.Server, *capture) {
	t.Helper()
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.method = r.Method
		cap.path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &cap.body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

func cloudTestAdapter(t *testing.T, baseURL string) *adapter {
	t.Helper()
	client, err := gobitbucket.NewAPITokenAuthWithBaseUrlStr("e@x", "tok", baseURL)
	asserts.NoError(t, err, "cloud client")
	return &adapter{fl: &cloudFlavor{client: client}}
}

func TestSendCloudPullRequest(t *testing.T) {
	srv, cap := recordingServer(t, http.StatusCreated)
	a := cloudTestAdapter(t, srv.URL)

	err := a.Send(context.Background(), "myws/myrepo!42", "hello", core.SendOptions{})
	asserts.NoError(t, err, "send")
	asserts.Equal(t, cap.method, http.MethodPost, "POST")
	asserts.Equal(t, cap.path, "/repositories/myws/myrepo/pullrequests/42/comments", "PR comment URL")
	content, _ := cap.body["content"].(map[string]any)
	asserts.Equal(t, content["raw"], "hello", "content.raw carries the text")
	_, hasParent := cap.body["parent"]
	asserts.False(t, hasParent, "plain send omits parent")
}

func TestSendCloudPullRequestThreaded(t *testing.T) {
	srv, cap := recordingServer(t, http.StatusCreated)
	a := cloudTestAdapter(t, srv.URL)

	err := a.SendThreaded(context.Background(), "myws/myrepo!42", "1001", "reply")
	asserts.NoError(t, err, "send threaded")
	parent, ok := cap.body["parent"].(map[string]any)
	asserts.True(t, ok, "parent set")
	asserts.Equal(t, parent["id"].(float64), float64(1001), "parent.id is the reply target")
}

func TestSendCloudIssue(t *testing.T) {
	srv, cap := recordingServer(t, http.StatusCreated)
	a := cloudTestAdapter(t, srv.URL)

	err := a.Send(context.Background(), "myws/myrepo#7", "issue reply", core.SendOptions{})
	asserts.NoError(t, err, "send")
	asserts.Equal(t, cap.path, "/repositories/myws/myrepo/issues/7/comments", "issue comment URL")
	content, _ := cap.body["content"].(map[string]any)
	asserts.Equal(t, content["raw"], "issue reply", "content.raw carries the text")
}

func TestSendDataCenterPullRequest(t *testing.T) {
	srv, cap := recordingServer(t, http.StatusCreated)
	a := &adapter{
		dataCenter: true,
		fl:         &serverFlavor{baseURL: srv.URL + dataCenterAPIPath, http: srv.Client(), applyAuth: func(*http.Request) {}},
	}

	err := a.Send(context.Background(), "PROJ/myrepo!42", "dc hello", core.SendOptions{})
	asserts.NoError(t, err, "send")
	asserts.Equal(t, cap.method, http.MethodPost, "POST")
	asserts.Equal(t, cap.path, dataCenterAPIPath+"/projects/PROJ/repos/myrepo/pull-requests/42/comments", "DC PR comment URL")
	asserts.Equal(t, cap.body["text"], "dc hello", "DC body uses text, not content.raw")
	_, hasContent := cap.body["content"]
	asserts.False(t, hasContent, "DC body has no content.raw")
}

func TestSendDataCenterThreaded(t *testing.T) {
	srv, cap := recordingServer(t, http.StatusCreated)
	a := &adapter{
		dataCenter: true,
		fl:         &serverFlavor{baseURL: srv.URL + dataCenterAPIPath, http: srv.Client(), applyAuth: func(*http.Request) {}},
	}

	err := a.SendThreaded(context.Background(), "PROJ/myrepo!42", "3003", "reply")
	asserts.NoError(t, err, "send threaded")
	parent, ok := cap.body["parent"].(map[string]any)
	asserts.True(t, ok, "parent set")
	asserts.Equal(t, parent["id"].(float64), float64(3003), "parent.id threads the reply")
}

// A "#" issue channel ID on a Data Center bot is rejected: Data Center has no
// issue tracker.
func TestSendDataCenterIssueRejected(t *testing.T) {
	a := &adapter{dataCenter: true, fl: &serverFlavor{}}
	err := a.Send(context.Background(), "PROJ/myrepo#7", "x", core.SendOptions{})
	asserts.ErrorIs(t, err, ErrBadChannelID, "issue on DC rejected")
}

func TestSendBadChannelID(t *testing.T) {
	a := &adapter{fl: &serverFlavor{}}
	err := a.Send(context.Background(), "no-sigil", "x", core.SendOptions{})
	asserts.ErrorIs(t, err, ErrBadChannelID, "bad channel id")
}

// An API failure is wrapped and reported, not swallowed.
func TestSendAPIError(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusInternalServerError)
	a := &adapter{
		dataCenter: true,
		fl:         &serverFlavor{baseURL: srv.URL + dataCenterAPIPath, http: srv.Client(), applyAuth: func(*http.Request) {}},
	}
	err := a.Send(context.Background(), "PROJ/myrepo!42", "x", core.SendOptions{})
	asserts.Error(t, err, "API error surfaces")
}
