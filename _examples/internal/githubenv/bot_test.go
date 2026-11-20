package githubenv

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lao/botbooter/github"
)

func TestNewBot(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "test.pem")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, token, keyFile string
		want                 error
	}{
		{"no credentials", "", "", github.ErrMissingConfig},
		{"both modes", "token", file, github.ErrAmbiguousAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_TOKEN", tc.token)
			t.Setenv("GITHUB_PRIVATE_KEY_FILE", tc.keyFile)
			t.Setenv("GITHUB_APP_ID", "7")
			t.Setenv("GITHUB_INSTALLATION_ID", "11")
			_, err := NewBot(github.Config{WebhookSecret: "test", Addr: ":0"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}

	for _, tc := range []struct{ name, token, keyFile, appID, installID, wantError string }{
		{"token", "token", "", "", "", ""},
		{"app", "", file, "7", "11", ""},
		{"missing file", "", file + ".missing", "bad", "bad", "read GITHUB_PRIVATE_KEY_FILE"},
		{"bad app ID", "", file, "bad", "bad", "parse GITHUB_APP_ID"},
		{"bad installation ID", "", file, "7", "bad", "parse GITHUB_INSTALLATION_ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_TOKEN", tc.token)
			t.Setenv("GITHUB_PRIVATE_KEY_FILE", tc.keyFile)
			t.Setenv("GITHUB_APP_ID", tc.appID)
			t.Setenv("GITHUB_INSTALLATION_ID", tc.installID)
			bot, err := NewBot(github.Config{WebhookSecret: "test", Addr: "127.0.0.1:0"})
			if tc.wantError != "" {
				if err == nil || !strings.HasPrefix(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want %q", err, tc.wantError)
				}
				if tc.name == "missing file" && !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("missing file error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if github.Client(bot) == nil {
				t.Fatal("missing GitHub client")
			}
		})
	}
}
