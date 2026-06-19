package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"bucks/internal/tui"
)

// startLaunchGateway runs BUCKS's Telegram gateway in the BACKGROUND for a normal `bucks`
// launch, so the bot the owner configured during setup actually responds — without them
// having to know to run `bucks --daemon` (the gap that left a required, validated bot token
// doing nothing on every normal launch, and the owner's remote /halt kill switch dead).
//
// It is tied to the launch's lifetime: the returned stop() cancels the long-poll loop and
// waits for it to drain. The caller (runDashboard) defers stop() so the gateway shuts down
// cleanly when the dashboard TUI exits.
//
// Lifetime/safety properties:
//   - No saved token -> a true no-op (returns a stop that does nothing); the dashboard opens
//     normally with nothing to poll.
//   - Gateway logs go to a FILE next to the config (gateway.log), NEVER the terminal, so they
//     can't corrupt the live bubbletea dashboard that owns the TTY. A test injects io.Discard.
//   - A start error is logged to that file, never fatal to the TUI — the dashboard always
//     opens even if the remote can't be reached.
//   - It reuses the SAME assembleDaemon wiring the `--daemon` path uses (one poller, fanned to
//     commands + approvals), so the launch bot and the daemon bot behave identically.
//
// opts let a test inject the apiBase + HTTP client (hermetic mock Telegram) and a log writer.
func startLaunchGateway(configPath string, r tui.SetupResult, opts ...DaemonOption) func() {
	if strings.TrimSpace(r.TelegramToken) == "" {
		return func() {} // nothing configured to poll
	}

	cfg := daemonConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	// Terminal-safe logging: unless a test injected a writer, send gateway logs to a file
	// next to the config so they never write to the TTY the dashboard owns.
	logw := cfg.logw
	if logw == nil {
		logw = openGatewayLog(configPath)
	}
	logf := func(format string, a ...any) { fmt.Fprintf(logw, format+"\n", a...) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		trustedChatID := loadTrustedChatID(configPath)
		asm, err := assembleDaemon(configPath, r.TelegramToken, trustedChatID, r, cfg, logf)
		if err != nil {
			logf("bucks gateway: could not start on launch: %v", err)
			return
		}
		logf("bucks gateway: running (started with the dashboard) — reach BUCKS on Telegram")
		if err := asm.gateway.Run(ctx); err != nil && ctx.Err() == nil {
			// A real failure (not our own shutdown) — record it; never crash the TUI.
			logf("bucks gateway: stopped with error: %v", err)
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// openGatewayLog opens (append) a gateway.log next to the config for the launch gateway's
// diagnostics, or returns io.Discard if it can't be created — the dashboard must open
// regardless, and a missing log is never fatal.
func openGatewayLog(configPath string) io.Writer {
	dir := filepath.Dir(configPath)
	if dir == "" {
		return io.Discard
	}
	f, err := os.OpenFile(filepath.Join(dir, "gateway.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return io.Discard
	}
	return f
}
