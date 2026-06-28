package botbooter_test

// Real-platform endurance smokes. Platform rate limits cap inbound volume far
// below "big traffic", so these are NOT throughput tests: they keep a bot
// connected for several minutes against a live workspace, drive a paced trickle
// of messages, and verify the bot stays connected, shuts down cleanly, and leaks
// no goroutines. They are gated behind an env var and skipped by default, exactly
// like TestConnectSlack_StartsAndStops. Run them via `make endurance` after
// exporting the BOTBOOTER_{SLACK,DISCORD}_* variables below.

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	slackapi "github.com/slack-go/slack"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/loadtest"
)

type enduranceConfig struct {
	duration time.Duration
	interval time.Duration
}

// runEnduranceSmoke runs bot for cfg.duration, calling drive every cfg.interval,
// and asserts the bot stays connected the whole time, shuts down cleanly when its
// context is canceled, and leaks no goroutines. It returns the number of drive
// calls made. Callers must not use t.Parallel: the goroutine-leak check reads the
// process-global goroutine count.
func runEnduranceSmoke(t *testing.T, bot *botbooter.Bot, cfg enduranceConfig, drive func(i int) error) int {
	t.Helper()
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- bot.Run(ctx) }()

	time.Sleep(2 * time.Second) // let the connection come up before driving

	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()
	deadline := time.After(cfg.duration)
	sent := 0
loop:
	for {
		select {
		case err := <-runErr:
			t.Fatalf("bot stopped before the endurance window elapsed: %v", err)
		case <-deadline:
			break loop
		case <-ticker.C:
			if err := drive(sent); err != nil {
				t.Errorf("driver post %d failed: %v", sent, err)
			}
			sent++
		}
	}

	cancel()
	select {
	case err := <-runErr:
		asserts.NoError(t, err, "bot shut down cleanly after the endurance window")
	case <-time.After(10 * time.Second):
		t.Fatal("bot.Run did not return within 10s of cancellation")
	}

	loadtest.AssertNoGoroutineLeak(t, before)
	return sent
}

func enduranceDuration(envName string, def time.Duration) time.Duration {
	if v := os.Getenv(envName); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s must be set for this endurance test", name)
	}
	return v
}

// TestEndurance_Slack_SustainedConnection holds a Socket Mode bot open while a
// separate identity posts into a shared channel. The driver MUST use a user token
// (xoxp): botbooter drops bot-authored messages (slack.isBotMessage), so a
// bot-token driver would never reach the handler.
func TestEndurance_Slack_SustainedConnection(t *testing.T) {
	if os.Getenv("BOTBOOTER_SLACK_ENDURANCE_TEST") == "" {
		t.Skip("set BOTBOOTER_SLACK_ENDURANCE_TEST=1 to run; performs real Slack network I/O for minutes")
	}
	appToken := requireEnv(t, "BOTBOOTER_SLACK_SUT_APP_TOKEN")
	botToken := requireEnv(t, "BOTBOOTER_SLACK_SUT_BOT_TOKEN")
	driverToken := requireEnv(t, "BOTBOOTER_SLACK_DRIVER_TOKEN")
	channelID := requireEnv(t, "BOTBOOTER_SLACK_CHANNEL_ID")

	bot := botbooter.InitAsSlackBot(appToken, botToken)
	var recv atomic.Int64
	asserts.NoError(t, bot.HandleFunc("^endurance", func(_ context.Context, _ *botbooter.Bot, _ *botbooter.Message) {
		recv.Add(1)
	}), "register handler")

	driver := slackapi.New(driverToken)
	cfg := enduranceConfig{
		duration: enduranceDuration("BOTBOOTER_SLACK_ENDURANCE_DURATION", 2*time.Minute),
		interval: 2 * time.Second, // stay under Slack's ~1 msg/sec/channel limit
	}
	sent := runEnduranceSmoke(t, bot, cfg, func(i int) error {
		_, _, err := driver.PostMessage(channelID, slackapi.MsgOptionText(fmt.Sprintf("endurance %d", i), false))
		return err
	})

	t.Logf("slack endurance: sent %d, received %d", sent, recv.Load())
	asserts.True(t, recv.Load() >= int64(sent/2), "bot kept receiving messages across the window")
}

// TestEndurance_Discord_SustainedConnection holds a Gateway bot open while a
// driver posts into a shared channel.
//
// Two driver modes (see discordDriverAuth):
//   - Bot driver (BOTBOOTER_DISCORD_DRIVER_TOKEN, default): botbooter drops every
//     bot-authored message (discord.go's m.Author.Bot guard), so the SUT receives
//     0 of these. The test hard-asserts only sustained connection, clean shutdown
//     and no goroutine leak (done by runEnduranceSmoke); the received count is
//     logged.
//   - User driver (BOTBOOTER_DISCORD_DRIVER_USER_TOKEN, opt-in): the driver posts
//     as a real user account, so the SUT receives the messages and the test also
//     asserts round-trip receipt. Automating a user account violates Discord's
//     Terms of Service, which is why this mode is an explicit opt-in.
func TestEndurance_Discord_SustainedConnection(t *testing.T) {
	if os.Getenv("BOTBOOTER_DISCORD_ENDURANCE_TEST") == "" {
		t.Skip("set BOTBOOTER_DISCORD_ENDURANCE_TEST=1 to run; performs real Discord network I/O for minutes")
	}
	sutToken := requireEnv(t, "BOTBOOTER_DISCORD_SUT_TOKEN")
	channelID := requireEnv(t, "BOTBOOTER_DISCORD_CHANNEL_ID")
	driverAuth, driverIsUser := discordDriverAuth(t)

	bot, err := botbooter.InitAsDiscordBot(sutToken)
	asserts.NoError(t, err, "init discord SUT")

	var recv atomic.Int64
	asserts.NoError(t, bot.HandleFunc("^endurance", func(_ context.Context, _ *botbooter.Bot, _ *botbooter.Message) {
		recv.Add(1)
	}), "register handler")

	driver, err := discordgo.New(driverAuth)
	asserts.NoError(t, err, "init discord driver")
	asserts.NoError(t, driver.Open(), "open discord driver")
	defer func() { _ = driver.Close() }()

	cfg := enduranceConfig{
		duration: enduranceDuration("BOTBOOTER_DISCORD_ENDURANCE_DURATION", 2*time.Minute),
		interval: 1500 * time.Millisecond, // stay under Discord's ~5 msg / 5s channel limit
	}
	sent := runEnduranceSmoke(t, bot, cfg, func(i int) error {
		_, sErr := driver.ChannelMessageSend(channelID, fmt.Sprintf("endurance %d", i))
		return sErr
	})

	if driverIsUser {
		t.Logf("discord endurance (user driver): sent %d, received %d", sent, recv.Load())
		asserts.True(t, recv.Load() >= int64(sent/2), "bot kept receiving messages across the window")
		return
	}
	t.Logf("discord endurance (bot driver): sent %d, received %d (received stays 0; bot-authored messages are dropped)", sent, recv.Load())
}

// discordDriverAuth resolves the driver's discordgo auth string and whether it is
// a user account. Setting BOTBOOTER_DISCORD_DRIVER_USER_TOKEN opts into driving
// with a real user account (passed without the "Bot " prefix), which lets the
// test assert round-trip receipt; otherwise it falls back to a bot token
// (BOTBOOTER_DISCORD_DRIVER_TOKEN), whose messages the SUT drops. Automating a
// user account violates Discord's Terms of Service, so this opt-in is deliberately
// explicit.
func discordDriverAuth(t *testing.T) (auth string, isUser bool) {
	t.Helper()
	if userToken := os.Getenv("BOTBOOTER_DISCORD_DRIVER_USER_TOKEN"); userToken != "" {
		return userToken, true
	}
	return "Bot " + requireEnv(t, "BOTBOOTER_DISCORD_DRIVER_TOKEN"), false
}
