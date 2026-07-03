package slack

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"

	"github.com/lao/botbooter/internal/asserts"
)

// slack-go only accepts the http client at construction (OptionHTTPClient), so
// only a white-box test in this package can inject a stub; an external test
// cannot, since the field is unexported.
type stubRoundTripper struct {
	status int
	body   string
}

func (s stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// The RoundTripper contract requires closing the request body.
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return &http.Response{
		StatusCode: s.status,
		Status:     fmt.Sprintf("%d %s", s.status, http.StatusText(s.status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Request:    req,
	}, nil
}

// capturingRoundTripper records the outgoing request body so a test can assert
// which form values slack-go serialized.
type capturingRoundTripper struct{ body string }

func (c *capturingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		c.body = string(b)
		_ = req.Body.Close()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		Request:    req,
	}, nil
}

// TestSendThreaded_IncludesThreadTS verifies the reply is posted in the reacted
// message's thread (thread_ts = replyToID).
func TestSendThreaded_IncludesThreadTS(t *testing.T) {
	rt := &capturingRoundTripper{}
	client := slackapi.New("xoxb-test", slackapi.OptionHTTPClient(&http.Client{Transport: rt}))
	a := &adapter{client: client}

	err := a.SendThreaded(context.Background(), "C123", "1700000000.000100", "hi there")

	asserts.NoError(t, err, "SendThreaded should succeed")
	asserts.True(t, strings.Contains(rt.body, "thread_ts=1700000000.000100"), "body carries thread_ts: "+rt.body)
	asserts.True(t, strings.Contains(rt.body, "channel=C123"), "body carries channel: "+rt.body)
}

// TestSend_SurfacesError verifies adapter.Send returns the Slack API's error.
func TestSend_SurfacesError(t *testing.T) {
	httpStub := &http.Client{Transport: stubRoundTripper{
		status: http.StatusOK,
		body:   `{"ok":false,"error":"invalid_auth"}`,
	}}
	client := slackapi.New("xoxb-test", slackapi.OptionHTTPClient(httpStub))
	a := &adapter{client: client}

	err := a.Send(context.Background(), "C123", "hello")

	asserts.Error(t, err, "Send should surface the Slack API error")
}
