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
	github.com/slack-go/slack v0.12.1
)

require (
	github.com/golang-jwt/jwt/v4 v4.5.2 // indirect
	github.com/google/go-querystring v1.2.0 // indirect
	github.com/gorilla/websocket v1.5.0 // indirect
	github.com/stretchr/testify v1.7.1 // indirect
	golang.org/x/crypto v0.7.0 // indirect
	golang.org/x/sys v0.6.0 // indirect
	gopkg.in/yaml.v3 v3.0.0-20210107192922-496545a6307b // indirect
)
