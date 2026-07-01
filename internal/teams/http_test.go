package teams

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

func TestGetJSON_Errors(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		a, err := newAdapter(validConfig())
		asserts.NoError(t, err, "newAdapter")
		err = a.getJSON(context.Background(), ":", &struct{}{})
		asserts.Error(t, err, "malformed URL should fail request creation")
	})

	t.Run("transport", func(t *testing.T) {
		a, err := newAdapter(validConfig())
		asserts.NoError(t, err, "newAdapter")
		a.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("transport failed")
		})}
		err = a.getJSON(context.Background(), "https://example.com", &struct{}{})
		asserts.Error(t, err, "transport failure should propagate")
	})

	t.Run("decode", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "not-json")
		}))
		defer srv.Close()
		a, err := newAdapter(validConfig())
		asserts.NoError(t, err, "newAdapter")
		err = a.getJSON(context.Background(), srv.URL, &struct{}{})
		asserts.Error(t, err, "invalid JSON should fail decoding")
	})
}
