package main

// daemon.go — the always-on `bucks --daemon` path. It assembles BUCKS's single Telegram
// long-poll gateway so the operator can reach the trader from anywhere (/status, /halt,
// /resume, …) with no terminal attached, and it shuts down gracefully on a signal.
//
// What it wires (one poller, fanned out by a Mux — the 409 tap-drop lesson):
//
//   getUpdates  ─▶  gateway.Run  ─▶  Mux ┬─ Text     ─▶ CommandRouter (operator commands)
//                                        └─ Callback ─▶ ApprovalRegistry (Approve/Deny taps)
//
//   sendMessage ◀─  TelegramSender  ◀──  both the CommandRouter (replies) and the
//                                        ApprovalRegistry (keyboards) post through it.
//
// The TelegramChannel (routine alerts/reports, wrapped in QuietChannel) has its Approver
// SET to that SAME registry, so an above-band approval the trade loop raises resolves from
// the one gateway stream — never a second poller. The live trade loop that would drive the
// channel lands later; this slice stands up the gateway and the durable command surface.
//
// HONESTY: with the live loop not yet running, /status and /summary report the DURABLE +
// CONFIG state (halt state from the kill switch, mode/broker/equity from the loaded setup)
// — never invented positions or P&L.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"bucks/internal/channel"
	"bucks/internal/gateway"
	"bucks/internal/orders"
	"bucks/internal/risk"
	"bucks/internal/secrets"
	"bucks/internal/tui"
)

// daemonCommandContext is the live CommandContext: it backs /status, /halt, /resume, and
// /summary with the DURABLE kill switch and the loaded config. It implements
// gateway.CommandContext. It holds no trade-loop state because none runs yet; the fields
// it reports are durable (the kill switch) or config-derived (mode/broker/equity), so it
// can never fabricate positions or P&L.
type daemonCommandContext struct {
	ks      *risk.KillSwitch
	mode    string         // "paper" or "live", from the loaded setup
	broker  string         // the configured broker kind, from the loaded setup
	capital orders.Decimal // the playbook capital (account equity until the live loop reports real equity)
}

// newDaemonCommandContext builds the live command context from the durable kill switch and
// the loaded setup. Mode is paper unless the setup armed live; broker is the first
// configured broker kind; capital is the playbook capital (the honest equity figure until
// a live feed exists).
func newDaemonCommandContext(ks *risk.KillSwitch, r tui.SetupResult) *daemonCommandContext {
	mode := "paper"
	if r.Live {
		mode = "live"
	}
	broker := ""
	if len(r.Brokers) > 0 {
		broker = string(r.Brokers[0].Kind)
	}
	return &daemonCommandContext{
		ks:      ks,
		mode:    mode,
		broker:  broker,
		capital: r.Playbook.Capital,
	}
}

// Halt trips the durable, MANUAL kill switch (operator-initiated /halt). The durable write
// happens before it returns, so the halt survives a restart.
func (c *daemonCommandContext) Halt(reason string) error {
	return c.ks.Halt(reason, risk.HaltManual)
}

// Resume clears the durable kill switch — the only exit from a halt, an explicit operator
// action (/resume).
func (c *daemonCommandContext) Resume() error {
	return c.ks.Clear()
}

// Status reports the at-a-glance state for /status: the live halt state + reason from the
// durable kill switch, and the mode/broker/equity from the loaded config. Equity is the
// playbook capital rendered with Decimal.String() (no float), the honest figure until the
// live loop reports real account equity.
func (c *daemonCommandContext) Status() gateway.StatusInfo {
	halted, reason := c.ks.IsHalted()
	return gateway.StatusInfo{
		Halted:     halted,
		HaltReason: reason,
		Mode:       c.mode,
		Broker:     c.broker,
		Equity:     c.capital.String(),
	}
}

// Report returns the snapshot for /summary and /positions. Until the live trade loop
// exists, it carries the real equity (the playbook capital) and a flat book — it does NOT
// fabricate positions or P&L. The live loop replaces this with real reports later.
func (c *daemonCommandContext) Report() channel.Report {
	return channel.Report{
		Equity:       c.capital,
		RealizedPL:   orders.ZeroDecimal,
		UnrealizedPL: orders.ZeroDecimal,
		Positions:    nil, // flat — honest, not a stub
	}
}

// daemonConfig holds the injectable seams runDaemon needs to be testable: the apiBase
// (an httptest URL in tests, the real Telegram base in production), the HTTP client, and a
// signal-free run hook used by the e2e test to drive the gateway directly. Production
// leaves them zero and runDaemon fills the real values.
type daemonConfig struct {
	apiBase     string           // overrides the Telegram base (tests inject httptest.URL)
	httpClient  *http.Client     // overrides the default client (tests inject srv.Client())
	logw        io.Writer        // where the plain start/stop lines go (defaults to os.Stdout)
	secretOpts  []secrets.Option // secrets-store backend opts; empty in prod (keychain), ForceFileBackend in tests
	confirmLive bool             // --live: arm a REAL-MONEY venue this session (default false = paper/monitor-only)
}

// DaemonOption configures runDaemon's injectable seams.
type DaemonOption func(*daemonConfig)

// withApiBase overrides the Telegram apiBase (https://api.telegram.org/bot<token>) with a
// test server URL so the e2e test never touches real Telegram.
func withApiBase(base string) DaemonOption {
	return func(c *daemonConfig) {
		if base != "" {
			c.apiBase = base
		}
	}
}

// withHTTPClient injects the HTTP client used by BOTH the poller and the sender so a test
// can point them at its httptest server.
func withHTTPClient(client *http.Client) DaemonOption {
	return func(c *daemonConfig) {
		if client != nil {
			c.httpClient = client
		}
	}
}

// withLogWriter redirects the plain start/stop lines (tests capture them; production uses
// os.Stdout).
func withLogWriter(w io.Writer) DaemonOption {
	return func(c *daemonConfig) {
		if w != nil {
			c.logw = w
		}
	}
}

// withSecretOpts injects the secrets-store backend options used to load the saved config.
// Production passes none (the OS keychain is preferred, exactly as the wizard saves);
// tests pass secrets.ForceFileBackend() so the load reads the same hermetic file the
// hermetic persistSetup wrote — never the developer's real keychain.
func withSecretOpts(opts ...secrets.Option) DaemonOption {
	return func(c *daemonConfig) {
		c.secretOpts = append(c.secretOpts, opts...)
	}
}

// withConfirmLive sets the per-session real-money confirmation (the --live flag). Default
// false keeps a live-configured trader in SAFE monitor/paper mode until deliberately armed.
func withConfirmLive(v bool) DaemonOption {
	return func(c *daemonConfig) { c.confirmLive = v }
}

// runDaemon is the testable core of `bucks --daemon`. It loads the saved setup, opens the
// durable kill switch, assembles the single Telegram gateway (one poller, fanned to the
// command router + approval registry), wires the channel's approver to that registry, and
// runs the long-poll loop until ctx is canceled (graceful shutdown).
//
// With NO Telegram token configured it does NOT crash: it prints a clear message and
// returns cleanly (there is nothing to poll). Production passes the real Telegram apiBase
// and the process context; tests inject an httptest apiBase + client and cancel the ctx.
func runDaemon(ctx context.Context, configPath string, opts ...DaemonOption) error {
	cfg := daemonConfig{logw: os.Stdout}
	for _, opt := range opts {
		opt(&cfg)
	}
	logf := func(format string, args ...any) {
		fmt.Fprintf(cfg.logw, format+"\n", args...)
	}

	// 1. Load the saved setup (playbook + secrets). The passphrase unlocks the file
	//    backend on a headless box (empty when a system keychain is in use).
	r, err := LoadSetup(configPath, passphraseFromEnv(), cfg.secretOpts...)
	if err != nil {
		return fmt.Errorf("daemon: load setup: %w", err)
	}

	// 2. No Telegram token -> nothing to poll. Say so plainly and exit cleanly; do NOT
	//    crash or busy-loop. The trader can still run paper, just without the remote.
	token := r.TelegramToken
	if token == "" {
		logf("bucks: no Telegram token configured — gateway not started. Set one with `bucks` (the guided setup) to reach BUCKS on Telegram.")
		return nil
	}

	// 3. The operator's trusted chat id, from BUCKS_TELEGRAM_CHAT_ID. This ONE value is the
	//    gateway's command-trust gate AND is passed explicitly into the routine channel
	//    (see assembleDaemon -> buildDaemonChannel), so the two can never disagree — no
	//    second, divergent config field. Without it the command router fails CLOSED (trusts
	//    no one), which is correct, but we warn so the operator can fix it.
	trustedChatID := loadTrustedChatID(configPath)
	if trustedChatID == 0 {
		logf("bucks: no operator paired yet — message your BUCKS bot once and that chat becomes the operator (remembered after, no env var needed).")
	}

	// 4. Assemble the gateway + the single-poller wiring (one place, so the assembly is
	//    testable without running the loop — see assembleDaemon).
	asm, err := assembleDaemon(configPath, token, trustedChatID, r, cfg, logf)
	if err != nil {
		return err
	}

	// 5. Start the trade loop in the background so the saved keys are actually USED: it watches
	//    the real account, enforces the drawdown gate + the operator's kill switch (the SAME ks
	//    the gateway's /halt trips — they must share one instance, since IsHalted reads in-memory
	//    state), and — once a trading policy is configured — places risk-managed orders.
	//    Monitor-only + paper by default; real money ONLY with --live. It routes alerts/approvals
	//    through the gateway's channel (no second poller). Stopped when the daemon shuts down.
	var loopCh channel.Channel = channel.NewMockChannel()
	if asm.channel != nil {
		loopCh = asm.channel
	}
	stopLoop := startTradeLoop(configPath, r, loopCh, cfg.confirmLive, asm.ks, logf)
	defer stopLoop()

	// 6. Graceful shutdown: Run returns only when ctx is canceled. The caller (production
	//    main, or the e2e test) owns the cancellation — production via signal.NotifyContext,
	//    the test via an explicit cancel. We log start + stop plainly.
	logf("BUCKS gateway running — reach it on Telegram (/status, /halt, /resume, /summary, /positions, /help)")
	err = asm.gateway.Run(ctx)
	logf("bucks: gateway stopped.")
	return err
}

// assembledDaemon is the wired-but-not-yet-running gateway and its collaborators. Returning
// it (rather than burying the wiring inside runDaemon) lets a test assert the single-poller
// wiring directly — most importantly that the operator channel's Approver IS the gateway's
// approval registry (so an above-band approval resolves from the ONE gateway stream, never
// a second poller).
type assembledDaemon struct {
	gateway  *gateway.Gateway
	registry *gateway.ApprovalRegistry
	channel  *channel.QuietChannel // the operator channel the trade loop sends through (nil if env-less)
	ks       *risk.KillSwitch      // the durable kill switch — SHARED with the trade loop so /halt stops it
}

// assembleDaemon builds the always-on gateway: the durable kill switch, the outbound
// sender, the command router + approval registry behind the Mux, the operator channel with
// its Approver set to that SAME registry, and the single long-poll Gateway. It does NOT run
// the loop — runDaemon does — so the assembly is unit-testable.
func assembleDaemon(configPath, token string, trustedChatID int64, r tui.SetupResult, cfg daemonConfig, logf func(string, ...any)) (*assembledDaemon, error) {
	// The apiBase carries the bot token. Tests override it with an httptest URL.
	apiBase := cfg.apiBase
	if apiBase == "" {
		apiBase = "https://api.telegram.org/bot" + token
	}

	// Durable kill switch next to the config (same path the paper-smoke boot uses), so an
	// operator /halt survives a restart.
	ksPath := filepath.Join(filepath.Dir(configPath), "killswitch.json")
	ks, err := risk.Open(ksPath)
	if err != nil {
		return nil, fmt.Errorf("daemon: open kill switch: %w", err)
	}

	// Outbound sender (command replies + approval keyboards) on the same apiBase/client.
	senderOpts := []gateway.SenderOption{}
	if cfg.httpClient != nil {
		senderOpts = append(senderOpts, gateway.WithSenderHTTPClient(cfg.httpClient))
	}
	sender := gateway.NewTelegramSender(apiBase, senderOpts...)

	// The two handlers behind the Mux: the command router (text) and the approval registry
	// (callback taps). The registry posts keyboards to the operator chat.
	cmdCtx := newDaemonCommandContext(ks, r)
	cmdOpts := []gateway.CommandOption{gateway.WithCommandLogger(logf)}
	if trustedChatID == 0 {
		// No operator configured yet — enable opt-in first-message pairing: the first chat to
		// message becomes the operator and is PERSISTED next to the config, so a wizard-only
		// owner never needs to set BUCKS_TELEGRAM_CHAT_ID. Fail-closed if the persist fails.
		cmdOpts = append(cmdOpts, gateway.WithPairing(func(id int64) bool {
			if err := saveTrustedChatID(configPath, id); err != nil {
				logf("bucks: pairing could not persist chat %d: %v", id, err)
				return false
			}
			logf("bucks: paired — chat %d now controls BUCKS (remembered).", id)
			return true
		}))
	}
	router := gateway.NewCommandRouter(trustedChatID, cmdCtx, sender, cmdOpts...)
	registry := gateway.NewApprovalRegistry(sender, gateway.WithChatID(trustedChatID))
	mux := &gateway.Mux{Text: router, Callback: registry}

	// The operator channel for routine alerts/reports the trade loop will use later. Its
	// Approver is the SAME registry, so an above-band approval resolves from the one gateway
	// stream (no second poller). Wrapped in QuietChannel so routine traffic respects quiet
	// hours. If the live channel can't be built (its env not set), we proceed without it —
	// the gateway itself does not depend on it, and the trade loop isn't running yet.
	var quiet *channel.QuietChannel
	if liveCh, cerr := buildDaemonChannel(cfg.httpClient, token, trustedChatID); cerr == nil {
		liveCh.SetApprover(registry) // single-poller wiring: channel approvals route through the gateway registry
		quiet = channel.NewQuietChannel(liveCh).WithLogf(logf)
	} else {
		logf("bucks: routine operator channel not started (%v) — gateway commands/approvals still run.", cerr)
	}

	// The single gateway: the ONLY getUpdates owner. Inject the test client; persist the
	// offset next to the config so a restart resumes where it left off.
	offsets := gateway.NewOffsetStore(filepath.Join(filepath.Dir(configPath), "telegram_offset.json"))
	gwOpts := []gateway.Option{gateway.WithLogger(logf)}
	if cfg.httpClient != nil {
		gwOpts = append(gwOpts, gateway.WithHTTPClient(cfg.httpClient))
	}
	gw := gateway.NewGateway(apiBase, offsets, mux, gwOpts...)

	return &assembledDaemon{gateway: gw, registry: registry, channel: quiet, ks: ks}, nil
}

// envTelegramChatID is the operator's chat id env var — the SAME name the live
// TelegramChannel reads, so the daemon's trusted-chat gate and the channel's destination
// can never disagree (one source of truth, no invented config field).
const envTelegramChatID = "BUCKS_TELEGRAM_CHAT_ID"

// trustedChatIDFromEnv parses BUCKS_TELEGRAM_CHAT_ID into the operator's trusted chat id.
// An unset or unparseable value yields 0, which makes the CommandRouter fail CLOSED (trust
// no one) — never fail open on a bad config.
func trustedChatIDFromEnv() int64 {
	raw := strings.TrimSpace(os.Getenv(envTelegramChatID))
	if raw == "" {
		return 0
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// buildDaemonChannel constructs the live TelegramChannel for routine alerts/reports the
// trade loop will use later. It is fed the SAME token the gateway uses — the wizard-saved
// r.TelegramToken from the encrypted secrets store — and the SAME trusted chat id, passed
// explicitly so a wizard-only operator (who never exported BUCKS_TELEGRAM_BOT_TOKEN) still
// gets a channel that can never diverge from the gateway. The channel falls back to its env
// vars only when these are empty. If neither yields a token/chat id it returns an error the
// caller treats as "no routine channel this run" — the gateway does not depend on it. The
// injected HTTP client (when present) points it at the test server.
func buildDaemonChannel(client *http.Client, token string, trustedChatID int64) (*channel.TelegramChannel, error) {
	opts := []channel.TelegramOption{
		channel.WithToken(token),
		channel.WithChatID(trustedChatID),
	}
	if client != nil {
		opts = append(opts, channel.WithHTTPClient(client))
	}
	return channel.NewTelegramChannel(opts...)
}

// runDaemonProcess is the production entry the `--daemon` branch calls. It installs the
// signal-aware context (Ctrl-C / SIGTERM -> graceful shutdown) and delegates to runDaemon.
// Splitting it out keeps runDaemon free of os/signal so the e2e test drives the real
// assembly with an explicit cancel instead of a process signal.
func runDaemonProcess(configPath string, confirmLive bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runDaemon(ctx, configPath, withConfirmLive(confirmLive))
}
