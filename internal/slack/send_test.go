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

// TestSendThreaded_IncludesThreadTS verifies the reply is posted in the reacted
// message's thread (thread_ts = replyToID).
func TestSendThreaded_IncludesThreadTS(t *testing.T) {
	rt := &capturingRoundTripper{}
	client := slackapi.New("xoxb-test", slackapi.OptionHTTPClient(&http.Client{Transport: rt}))
	a := &adapter{client: client}

	err := a.SendThreaded(context.Background(), "C123", "1700000000.000100", "hi there")

	asserts.NoError(t, err, "SendThreaded should succeed")
	asserts.Equal(t, rt.form.Get("thread_ts"), "1700000000.000100", "body carries thread_ts")
	asserts.Equal(t, rt.form.Get("channel"), "C123", "body carries channel")
}

// TestSendThreaded_Error verifies SendThreaded surfaces the Slack API's error
// rather than swallowing it.
func TestSendThreaded_Error(t *testing.T) {
	httpStub := &http.Client{Transport: stubRoundTripper{
		status: http.StatusOK,
		body:   `{"ok":false,"error":"invalid_auth"}`,
	}}
	client := slackapi.New("xoxb-test", slackapi.OptionHTTPClient(httpStub))
	a := &adapter{client: client}

	err := a.SendThreaded(context.Background(), "C123", "1700000000.000100", "hi")

	asserts.Error(t, err, "SendThreaded should surface the Slack API error")
}

// TestSend_SurfacesError verifies adapter.Send returns the Slack API's error.
func TestSend_SurfacesError(t *testing.T) {
	httpStub := &http.Client{Transport: stubRoundTripper{
		status: http.StatusOK,
		body:   `{"ok":false,"error":"invalid_auth"}`,
	}}
	client := slackapi.New("xoxb-test", slackapi.OptionHTTPClient(httpStub))
	a := &adapter{client: client}

	err := a.Send(context.Background(), "C123", "hello", core.SendOptions{})

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

// TestSend_Threading verifies the resolved SendOptions map to thread_ts: an
// InReplyTo threaded message replies inside its thread (thread_ts = ReplyToID),
// a top-level message replies in the channel with no thread_ts, and a raw
// WithThreadID is used verbatim.
func TestSend_Threading(t *testing.T) {
	newAdapter := func(rt *capturingRoundTripper) *adapter {
		client := slackapi.New("xoxb-test", slackapi.OptionHTTPClient(&http.Client{Transport: rt}))
		return &adapter{client: client}
	}

	t.Run("InReplyToThreadedMessageRepliesInThread", func(t *testing.T) {
		rt := &capturingRoundTripper{}
		a := newAdapter(rt)

		err := a.Send(context.Background(), "C1", "hi",
			core.SendOptions{ReplyTo: &core.Message{ChannelID: "C1", ID: "200.2", ReplyToID: "100.1"}})

		asserts.NoError(t, err, "Send")
		asserts.Equal(t, rt.form.Get("thread_ts"), "100.1", "thread_ts should be the thread root")
		asserts.Equal(t, rt.form.Get("channel"), "C1", "channel")
	})

	t.Run("InReplyToTopLevelMessageRepliesInChannel", func(t *testing.T) {
		rt := &capturingRoundTripper{}
		a := newAdapter(rt)

		err := a.Send(context.Background(), "C1", "hi",
			core.SendOptions{ReplyTo: &core.Message{ChannelID: "C1", ID: "200.2"}})

		asserts.NoError(t, err, "Send")
		asserts.Equal(t, rt.form.Get("thread_ts"), "", "a top-level message must not start a thread")
	})

	t.Run("WithThreadIDUsedVerbatim", func(t *testing.T) {
		rt := &capturingRoundTripper{}
		a := newAdapter(rt)

		err := a.Send(context.Background(), "C1", "hi", core.SendOptions{ThreadID: "999.9"})

		asserts.NoError(t, err, "Send")
		asserts.Equal(t, rt.form.Get("thread_ts"), "999.9", "raw ThreadID becomes thread_ts")
	})

	t.Run("ThreadIDWinsOverReplyTo", func(t *testing.T) {
		rt := &capturingRoundTripper{}
		a := newAdapter(rt)

		err := a.Send(context.Background(), "C1", "hi", core.SendOptions{
			ThreadID: "999.9",
			ReplyTo:  &core.Message{ChannelID: "C1", ID: "200.2", ReplyToID: "100.1"},
		})

		asserts.NoError(t, err, "Send")
		asserts.Equal(t, rt.form.Get("thread_ts"), "999.9", "explicit ThreadID wins over the ReplyTo anchor")
	})
}

// TestReply_RoutesThroughAdapter is the end-to-end guard: (*core.Bot).Reply on a
// real Slack adapter must reach the threaded Send path (thread_ts set), not a
// channel-root plain send.
func TestReply_RoutesThroughAdapter(t *testing.T) {
	rt := &capturingRoundTripper{}
	client := slackapi.New("xoxb-test", slackapi.OptionHTTPClient(&http.Client{Transport: rt}))
	bot := core.New(core.SlackBotType, &adapter{client: client})

	err := bot.Reply(context.Background(),
		&core.Message{ChannelID: "C1", ID: "200.2", ReplyToID: "100.1"}, "hi")

	asserts.NoError(t, err, "Reply")
	asserts.Equal(t, rt.form.Get("thread_ts"), "100.1", "Reply threaded the reply via Send options")
}
