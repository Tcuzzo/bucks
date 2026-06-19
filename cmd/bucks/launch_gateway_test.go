package main

import (
	"io"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"bucks/internal/secrets"
)

// TestLaunchGatewayServesBotOnNormalLaunch is the FINDING fix: the Telegram bot the owner
// configured in setup must respond on a NORMAL `bucks` launch — not only under `--daemon`.
// startLaunchGateway runs the gateway in the background for a loaded setup; here, pointed at
// a hermetic mock Telegram, it must serve the operator's /status command (posting a reply)
// just like the daemon does. Unsteered: it drives the real assembled gateway.
func TestLaunchGatewayServesBotOnNormalLaunch(t *testing.T) {
	const trustedChatID = int64(515151)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "launch-gw-pass"
	if err := persistSetup(validSetupResult(t), configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("persistSetup: %v", err)
	}
	t.Setenv("BUCKS_TELEGRAM_CHAT_ID", "515151")
	t.Setenv("BUCKS_TELEGRAM_BOT_TOKEN", "test-token")

	r, err := LoadSetup(configPath, pass, secrets.ForceFileBackend())
	if err != nil {
		t.Fatalf("LoadSetup: %v", err)
	}

	mock := newMockTelegram(trustedChatID)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	stop := startLaunchGateway(configPath, r,
		withApiBase(srv.URL),
		withHTTPClient(srv.Client()),
		withLogWriter(io.Discard),
	)
	defer stop()

	select {
	case <-mock.statusReplied:
		// The launch-started gateway served /status — the bot is alive on a normal launch.
	case <-time.After(5 * time.Second):
		t.Fatal("launch gateway did not serve the bot (/status) within 5s — Telegram still dead on normal launch")
	}
}

// TestLaunchGatewayNoTokenIsNoop proves a config WITHOUT a Telegram token starts no gateway
// and the returned stop() is a safe no-op (the dashboard still opens; nothing to poll).
func TestLaunchGatewayNoTokenIsNoop(t *testing.T) {
	r := validSetupResult(t)
	r.TelegramToken = ""
	stop := startLaunchGateway(filepath.Join(t.TempDir(), "bucks.yaml"), r)
	if stop == nil {
		t.Fatal("startLaunchGateway must always return a non-nil stop func")
	}
	stop() // must return immediately without blocking or panicking
}
