package slack

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"

	"github.com/lao/botbooter/internal/core"

	"github.com/lao/botbooter/internal/asserts"
)

// The adapter must satisfy the optional ThreadedSender so (*core.Bot).Reply
// routes to SendThreaded rather than the plain-Send fallback (guards the
// pointer-receiver method-set trap).
var _ core.ThreadedSender = (*adapter)(nil)

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

// capturingRoundTripper records the form values of the last request so a test
// can assert what was posted to the Web API.
type capturingRoundTripper struct {
	form url.Values
}

func (c *capturingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	_ = req.Body.Close()
	c.form, _ = url.ParseQuery(string(body))
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		Request:    req,
	}, nil
}

// TestSendThreaded_PassesThreadTS verifies a threaded message replies inside its
// thread (thread_ts = ReplyToID), while a top-level message replies in the
// channel with no thread_ts.
func TestSendThreaded_PassesThreadTS(t *testing.T) {
	t.Run("ThreadedMessageRepliesInThread", func(t *testing.T) {
		rt := &capturingRoundTripper{}
		client := slackapi.New("xoxb-test", slackapi.OptionHTTPClient(&http.Client{Transport: rt}))
		a := &adapter{client: client}

		err := a.SendThreaded(context.Background(),
			&core.Message{ChannelID: "C1", ID: "200.2", ReplyToID: "100.1"}, "hi")

		asserts.NoError(t, err, "SendThreaded")
		asserts.Equal(t, rt.form.Get("thread_ts"), "100.1", "thread_ts should be the thread root")
		asserts.Equal(t, rt.form.Get("channel"), "C1", "channel")
	})

	t.Run("TopLevelMessageRepliesInChannel", func(t *testing.T) {
		rt := &capturingRoundTripper{}
		client := slackapi.New("xoxb-test", slackapi.OptionHTTPClient(&http.Client{Transport: rt}))
		a := &adapter{client: client}

		err := a.SendThreaded(context.Background(),
			&core.Message{ChannelID: "C1", ID: "200.2"}, "hi")

		asserts.NoError(t, err, "SendThreaded")
		asserts.Equal(t, rt.form.Get("thread_ts"), "", "a top-level message must not start a thread")
	})
}

// TestReply_RoutesThroughAdapter is the end-to-end guard: (*core.Bot).Reply on a
// real Slack adapter must reach SendThreaded (via the runtime ThreadedSender
// assertion), not silently fall back to a channel-root Send.
func TestReply_RoutesThroughAdapter(t *testing.T) {
	rt := &capturingRoundTripper{}
	client := slackapi.New("xoxb-test", slackapi.OptionHTTPClient(&http.Client{Transport: rt}))
	bot := core.New(core.SlackBotType, &adapter{client: client})

	err := bot.Reply(context.Background(),
		&core.Message{ChannelID: "C1", ID: "200.2", ReplyToID: "100.1"}, "hi")

	asserts.NoError(t, err, "Reply")
	asserts.Equal(t, rt.form.Get("thread_ts"), "100.1", "Reply reached SendThreaded and threaded the reply")
}
