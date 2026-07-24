// HTTP plumbing shared by the adapter's outbound calls: a JSON GET helper and the
// response decoder that enforces status, size caps and keep-alive drain.

package teams

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// maxErrorBodyBytes caps how much of a non-2xx body is read into the error.
	maxErrorBodyBytes = 4 << 10 // 4 KiB
	// maxMetaBytes caps JWKS/OpenID/token JSON decoded from Microsoft. The Bot
	// Connector JWKS is large (~1 MB), so this must comfortably exceed it; the
	// generous ceiling is a memory-exhaustion backstop on trusted endpoints.
	maxMetaBytes = 4 << 20 // 4 MiB
)

func (a *adapter) getJSON(ctx context.Context, endpoint string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := decodeJSON(resp, v); err != nil {
		return fmt.Errorf("teams: GET %s: %w", endpoint, err)
	}
	return nil
}

// decodeJSON is the shared tail for the adapter's outbound HTTP calls: it treats a
// non-2xx status as an error (with a capped body snippet), decodes the body as JSON
// into v when v is non-nil, then drains the rest for keep-alive. Pass a nil v when
// there is no response body to decode (e.g. a reply POST).
func decodeJSON(resp *http.Response, v any) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if v != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetaBytes)).Decode(v); err != nil {
			return fmt.Errorf("decode body: %w", err)
		}
	}
	// Drain trailing bytes so the connection can be reused (keep-alive).
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
