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
