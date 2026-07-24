// Outbound reply path: POST a message to a conversation, and the cached
// client-credentials token that authorizes it.

package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lao/botbooter/internal/core"
)

const (
	// tokenScope requests an app-only token for the Bot Connector service.
	tokenScope = "https://api.botframework.com/.default" //nolint:gosec // OAuth scope URL, not a credential
	// tokenRefreshSkew refreshes the outbound token a little before it expires.
	tokenRefreshSkew = time.Minute
)

// ErrUnknownConversation is returned by Send when no inbound Activity has been
// seen for the target conversation — so no serviceUrl is known — or it was
// evicted from the conversation map. Callers doing proactive sends can
// errors.Is-branch it from transport failures.
var ErrUnknownConversation = errors.New("teams: unknown conversation")

type cachedToken struct {
	value  string
	expiry time.Time
}

// outboundActivity is the reply posted to the Bot Connector. from is required;
// recipient is omitted because replies are delivered by conversation id and a
// cached per-activity recipient would misattribute a concurrent sender's reply.
type outboundActivity struct {
	Type string         `json:"type"`
	Text string         `json:"text"`
	From channelAccount `json:"from"`
}

// Send posts a text reply to the conversation channelID. The serviceUrl and bot
// account come from the map populated on inbound Activities, so Send fails if no
// Activity has been seen for channelID yet. Teams has no thread anchor here, so
// SendOptions is ignored — a reply already lands in the originating conversation.
func (a *adapter) Send(ctx context.Context, channelID, text string, _ core.SendOptions) error {
	a.mu.Lock()
	conv, ok := a.convs[channelID]
	a.mu.Unlock()
	if !ok || conv.serviceURL == "" {
		return fmt.Errorf("%w %q (no inbound activity seen)", ErrUnknownConversation, channelID)
	}

	// from (the bot account) was captured from the inbound Activity's recipient.
	payload := outboundActivity{
		Type: "message",
		Text: text,
		From: conv.bot,
	}
	// channelAccount holds only strings, which encoding/json cannot reject.
	body, _ := json.Marshal(payload)

	endpoint := strings.TrimRight(conv.serviceURL, "/") + "/v3/conversations/" + url.PathEscape(channelID) + "/activities"

	// The cached token can go stale independently of its local expiry (e.g. after
	// an app-secret rotation); on 401/403, drop the cache and retry once.
	status, err := a.postActivity(ctx, endpoint, body)
	if err != nil && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		a.invalidateToken()
		_, err = a.postActivity(ctx, endpoint, body)
	}
	return err
}

// postActivity mints/uses the cached token and POSTs body to endpoint, returning
// the HTTP status (0 if the request never reached a response) and any error.
func (a *adapter) postActivity(ctx context.Context, endpoint string, body []byte) (int, error) {
	token, err := a.accessToken(ctx)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	// No response body to decode; decodeJSON just validates status and drains.
	status := resp.StatusCode
	if err := decodeJSON(resp, nil); err != nil {
		return status, fmt.Errorf("teams: send failed: %w", err)
	}
	return status, nil
}

// invalidateToken clears the cached outbound token so the next accessToken mints
// a fresh one.
func (a *adapter) invalidateToken() {
	a.mu.Lock()
	a.token = cachedToken{}
	a.mu.Unlock()
}

// cachedToken returns the cached outbound token when it is present and not within
// tokenRefreshSkew of expiry, reporting whether it is usable.
func (a *adapter) cachedToken() (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token.value != "" && time.Until(a.token.expiry) > tokenRefreshSkew {
		return a.token.value, true
	}
	return "", false
}

// accessToken returns a cached Bot Connector token, minting a fresh one via the
// client-credentials grant when the cache is empty or near expiry. The network
// call is made outside a.mu.
func (a *adapter) accessToken(ctx context.Context) (string, error) {
	if v, ok := a.cachedToken(); ok {
		return v, nil
	}

	// Serialize cold mints so a burst of concurrent Sends makes a single token
	// request; re-check the cache after winning tokenMu since a concurrent mint
	// may have already populated it (mirrors the JWKS fetchMu coalescing).
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	if v, ok := a.cachedToken(); ok {
		return v, nil
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {a.cfg.AppID},
		"client_secret": {a.cfg.AppPassword},
		"scope":         {tokenScope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := decodeJSON(resp, &out); err != nil {
		return "", fmt.Errorf("teams: token request: %w", err)
	}
	if out.AccessToken == "" {
		return "", errors.New("teams: token response missing access_token")
	}
	// Floor a missing/zero expires_in so a bad response can't collapse the cache
	// into minting a token on every Send.
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 300
	}

	a.mu.Lock()
	a.token = cachedToken{value: out.AccessToken, expiry: time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)}
	a.mu.Unlock()
	return out.AccessToken, nil
}
