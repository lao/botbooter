module github.com/lao/botbooter

go 1.25.0

// Pinned to the latest 1.25 patch release so CI (setup-go reads this file) and
// govulncheck run against a stdlib with all published fixes applied.
toolchain go1.25.11

require (
	github.com/bradleyfalzon/ghinstallation/v2 v2.19.0
	github.com/bwmarrin/discordgo v0.27.1
	github.com/go-telegram/bot v1.21.0
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/google/go-github/v88 v88.0.0
	github.com/gorilla/websocket v1.5.0
	github.com/mdp/qrterminal/v3 v3.2.1
	github.com/slack-go/slack v0.12.1
	gitlab.com/gitlab-org/api/client-go v1.46.0
	go.mau.fi/whatsmeow v0.0.0-20260622185415-5f04eac6dbbb
	google.golang.org/protobuf v1.36.11
	modernc.org/sqlite v1.53.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/beeper/argo-go v1.1.2 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/elliotchance/orderedmap/v3 v3.1.0 // indirect
	github.com/golang-jwt/jwt/v4 v4.5.2 // indirect
	github.com/google/go-querystring v1.2.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/hashicorp/go-retryablehttp v0.7.8 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/petermattis/goid v0.0.0-20260330135022-df67b199bc81 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rs/zerolog v1.35.1 // indirect
	github.com/vektah/gqlparser/v2 v2.5.27 // indirect
	go.mau.fi/libsignal v0.2.2 // indirect
	go.mau.fi/util v0.9.10 // indirect
	golang.org/x/crypto v0.53.0 // indirect
	golang.org/x/exp v0.0.0-20260611194520-c48552f49976 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/oauth2 v0.34.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/term v0.44.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	golang.org/x/time v0.14.0 // indirect
	modernc.org/libc v1.73.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	rsc.io/qr v0.2.0 // indirect
)
