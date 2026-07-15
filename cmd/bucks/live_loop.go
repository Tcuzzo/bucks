package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"bucks/internal/analyst"
	"bucks/internal/brokers"
	"bucks/internal/channel"
	"bucks/internal/harness"
	"bucks/internal/ledger"
	"bucks/internal/memory"
	"bucks/internal/playbook"
	"bucks/internal/risk"
	"bucks/internal/tui"
)

// seedRetryAttempts / seedRetryDelay bound the boot-seed retry so a SINGLE transient
// blip on broker.Positions() at process start does not needlessly refuse to trade
// (including plain paper monitoring). Vars so tests run fast.
var (
	seedRetryAttempts = 3
	seedRetryDelay    = 500 * time.Millisecond
)

type ledgerSeeder interface {
	Seed(symbol string, signedQty, avgPx ledger.Decimal)
	LoadState() (bool, error)
	SaveState(cursor time.Time) error
}

type ledgerSeedStateSaver interface {
	SaveSeedState(cursor time.Time, fills []brokers.Fill) error
}

// seedLedgerFromBroker reconciles open positions from the broker (the source of
// truth for holdings) into the FIFO cost basis on boot. A persistent
// Positions() failure (after a bounded retry) is returned so the caller can FAIL
// CLOSED — never silently seed an empty ledger, which would make the next close of a
// pre-existing position realize $0 and hide a real loss from the daily-loss budget.
func seedLedgerFromBroker(seeder ledgerSeeder, broker brokers.Broker) (bool, error) {
	return seedLedgerFromBrokerAt(seeder, broker, time.Now().UTC())
}

// seedLedgerFromBrokerAt seeds the first-boot FIFO cost basis and reports a COLD
// START: true when there is same-day broker activity that predates this first boot,
// whose realized P&L is therefore NOT in today's realized-loss budget. This is a
// bounded, first-boot-only limitation — a position closed before BUCKS first ran has
// no reconstructable entry basis (Positions() no longer holds it, and the venue fill
// stream alone can't price a prior-day open). Every later boot reloads durable
// cursor/basis and is exact. nowRef is the "today" reference for this check ONLY; the
// reconcile cursor itself stays venue-clock derived (skew-immune). A true return tells
// the caller to surface the loud first-boot notice; the seed itself is unaffected.
func seedLedgerFromBrokerAt(seeder ledgerSeeder, broker brokers.Broker, nowRef time.Time) (bool, error) {
	loaded, err := seeder.LoadState()
	if err != nil {
		return false, fmt.Errorf("cannot read persisted P&L ledger basis — refusing to trade with an unknown cost basis: %w", err)
	}
	if loaded {
		return false, nil
	}
	var positions []brokers.Position
	for attempt := 1; attempt <= seedRetryAttempts; attempt++ {
		if positions, err = broker.Positions(context.Background()); err == nil {
			break
		}
		if attempt < seedRetryAttempts && seedRetryDelay > 0 {
			time.Sleep(seedRetryDelay)
		}
	}
	if err != nil {
		return false, fmt.Errorf("cannot read broker positions to seed the P&L ledger after %d attempts — refusing to trade with an unseeded cost basis: %w", seedRetryAttempts, err)
	}
	seedFills, seedCursor, ferr := seedCursorFromBrokerFills(context.Background(), broker)
	if ferr != nil {
		return false, fmt.Errorf("cannot read broker fills to seed the PnL ledger; refusing to trade with an unreconciled cost basis: %w", ferr)
	}
	// COLD START: any seed fill dated on today's UTC day happened before this first
	// boot; its realized P&L is not reconstructed into today's budget. Flag it so the
	// caller tells the operator loudly (the budget starts fresh at boot). The day
	// boundary uses nowRef only — the cursor stays venue-clock derived below.
	coldStart := false
	if !nowRef.IsZero() {
		dayStart := startOfUTCDay(nowRef)
		for _, f := range seedFills {
			if !f.At.Before(dayStart) {
				coldStart = true
				break
			}
		}
	}
	cursor := seedCursor
	for _, p := range positions {
		seeder.Seed(p.Symbol, p.Qty, p.AvgEntryPx)
	}
	if seedSaver, ok := seeder.(ledgerSeedStateSaver); ok {
		if err := seedSaver.SaveSeedState(cursor.UTC(), seedFills); err != nil {
			return false, fmt.Errorf("cannot persist broker position seed for the PnL ledger; refusing to trade with a volatile cost basis: %w", err)
		}
		return coldStart, nil
	}
	if err := seeder.SaveState(cursor.UTC()); err != nil {
		return false, fmt.Errorf("cannot persist broker position seed for the PnL ledger; refusing to trade with a volatile cost basis: %w", err)
	}
	return coldStart, nil
}

func seedCursorFromBrokerFills(ctx context.Context, broker brokers.Broker) ([]brokers.Fill, time.Time, error) {
	fillReader, ok := broker.(brokers.FillReader)
	if !ok {
		return nil, firstBootNoFillCursor(), nil
	}
	fills, err := fillReader.FillsSince(ctx, time.Time{})
	if err != nil {
		return nil, time.Time{}, err
	}
	cursor := firstBootNoFillCursor()
	for _, f := range fills {
		if f.ID == "" {
			return nil, time.Time{}, fmt.Errorf("broker fill missing activity id for %s", f.Symbol)
		}
		if f.At.IsZero() {
			return nil, time.Time{}, fmt.Errorf("broker fill %s missing transaction time", f.ID)
		}
		if f.At.After(cursor) {
			cursor = f.At.UTC()
		}
	}
	return fills, cursor, nil
}

func firstBootNoFillCursor() time.Time {
	// With no venue FILL activity, there is no venue timestamp to reuse. Unix epoch
	// is a non-zero earliest sentinel for modern broker activity, so later real
	// fills still sort after the seed without consulting the BUCKS host clock.
	return time.Unix(0, 0).UTC()
}

// liveTraderResult is what buildLiveTrader produced. When Trader is nil the loop was NOT
// started (Reason says why, in plain English) — the SAFE outcome for a real-money venue that
// the owner has not explicitly confirmed this session. LiveActive is true only when a
// real-money venue is actually armed (so the caller can shout it in logs / the dashboard).
type liveTraderResult struct {
	Trader     *harness.Trader
	Broker     brokers.Broker
	Store      *memory.Store // realized-P&L ledger backing the daily-loss breaker (nil only when unbuilt)
	LiveActive bool
	Reason     string
}

// buildLiveTrader constructs the trade loop's Trader from the owner's saved setup, with the
// real broker built from their saved keys and the SAFETY GATE on real money wired in by
// construction:
//
//   - A PAPER venue (alpaca-paper) always runs — it cannot lose real money.
//   - A REAL-MONEY venue (alpaca-live) runs ONLY when confirmLive is true (a deliberate
//     per-session confirmation, e.g. `bucks --live`). Without it the loop is NOT started:
//     no live broker is built, no order can be placed, and the caller is told why. This is
//     the "never silently go live on boot" guarantee — persisting the arm only REMEMBERS the
//     intent; a real order needs the explicit confirmation too.
//
// The Trader is wired with the playbook's risk engine, the durable kill switch (next to the
// config), the operator channel (alerts/reports/approvals), and a per-trade auto/approve band
// sized from the playbook. LiveEnabled reflects whether real money is armed. ch is injected so
// the daemon passes its gateway-wired channel and tests pass a mock.
// ks is the SHARED durable kill switch. It MUST be the same instance the daemon's command
// context uses, because IsHalted reads in-memory state — so an operator /halt only stops the
// loop when both sides share one switch. A nil ks means "open my own" (standalone/tests).
func buildLiveTrader(r tui.SetupResult, configPath string, ch channel.Channel, confirmLive bool, ks *risk.KillSwitch) (liveTraderResult, error) {
	if len(r.Brokers) == 0 {
		return liveTraderResult{Reason: "no broker configured — run setup to add one"}, nil
	}
	creds := r.Brokers[0]

	// SAFETY GATE: a real-money venue requires an explicit per-session confirmation. Without
	// it, do not build the live broker or start the loop — there is nothing that can place a
	// real order. (Paper venues fall through and run freely.)
	if isLiveBroker(creds.Kind) && !confirmLive {
		return liveTraderResult{
			Reason: "configured for LIVE (real money) but not confirmed this session — staying safe; start live trading deliberately with `bucks --live`",
		}, nil
	}

	broker, err := brokerFromCreds(creds)
	if err != nil {
		return liveTraderResult{}, fmt.Errorf("live loop: %w", err)
	}

	if ks == nil {
		// Standalone use: open our own durable switch next to the config.
		ks, err = risk.Open(filepath.Join(filepath.Dir(configPath), "killswitch.json"))
		if err != nil {
			return liveTraderResult{}, fmt.Errorf("live loop: open kill switch: %w", err)
		}
	}

	// The realized-P&L ledger: a single-file SQLite store next to the config. The
	// broker activity-stream reconciler is the single authoritative accounting path:
	// it applies every unseen venue fill to FIFO basis and persists realized closes
	// here so the daily-loss breaker reads a REAL number that survives a restart.
	store, err := memory.Open(filepath.Join(filepath.Dir(configPath), "memory.sqlite"))
	if err != nil {
		return liveTraderResult{}, fmt.Errorf("live loop: open ledger store: %w", err)
	}
	// NOTE: reconcile-on-boot seeding (broker.Positions) happens at LOOP START in
	// startTradeLoop — a network call that must not run during pure construction.

	liveActive := isLiveBroker(creds.Kind) // true here only means: real venue AND confirmed
	trader, err := harness.NewTrader(harness.TraderConfig{
		StrategyName:    "bucks-live",
		Engine:          risk.NewEngine(r.Playbook.ToRiskConfig()),
		Broker:          broker,
		KillSwitch:      ks,
		Channel:         ch,
		Band:            bandFromPlaybook(r.Playbook),
		Market:          harness.Always24x7{}, // crypto-style; an equities calendar is a later refinement
		Now:             time.Now,
		HeartbeatEvery:  1 * time.Minute,
		ReportEvery:     15 * time.Minute,
		LiveEnabled:     liveActive,
		RealMoneyVenue:  isLiveBroker(creds.Kind), // arms the placement-time guard in placeOnBroker
		ApprovalTimeout: 5 * time.Minute,
	})
	if err != nil {
		_ = store.Close()
		return liveTraderResult{}, fmt.Errorf("live loop: construct trader: %w", err)
	}

	return liveTraderResult{Trader: trader, Broker: broker, Store: store, LiveActive: liveActive}, nil
}

// bandFromPlaybook sizes the per-trade auto/approve band from the owner's capital: trades up
// to ~5% notional and ~1% risk place automatically; anything larger asks the owner for
// approval in Telegram before it is placed. Floors keep the band sane on a tiny account.
func bandFromPlaybook(pb playbook.Playbook) harness.HybridBandConfig {
	return harness.HybridBandConfig{
		MaxAutoNotional:   fractionOrFloor(pb.Capital, "0.05", "500"),
		MaxAutoRiskAmount: fractionOrFloor(pb.Capital, "0.01", "50"),
	}
}

// (fractionOrFloor lives in boot.go — max(capital*frac, floor) in exact decimal.)

// buildDecider builds the trade loop's decision policy from the owner's setup. When a brain is
// configured (the wizard's LLM choice yields a backend), it returns the PLAYBOOK-DRIVEN decider
// — the bot derives its universe, picks, stop, and size from the owner's playbook, with the
// brain reasoning per symbol; the operator picks nothing. With no brain it falls back to
// monitor-only (the loop still watches the account + enforces safety, but cannot reason).
func buildDecider(r tui.SetupResult, broker brokers.Broker, logf func(string, ...any)) Decider {
	backends, err := configChatBackends(r)
	if err != nil || len(backends) == 0 {
		logf("trade loop: no brain configured — monitor-only (choose an LLM in setup to let BUCKS trade your playbook)")
		return monitorOnlyDecider()
	}
	an, aerr := analyst.New(r.Playbook, nil, backends...)
	if aerr != nil {
		logf("trade loop: brain build failed (%v) — monitor-only", aerr)
		return monitorOnlyDecider()
	}
	logf("trade loop: playbook-driven brain active — universe, picks, stops, and size all come from your playbook")
	return newPlaybookDecider(an, broker, r.Playbook)
}

// tradeLoopInterval is how often the live loop polls the account + asks the decider. A var so
// tests can shrink it; production polls every 15s.
var tradeLoopInterval = 15 * time.Second

// startTradeLoop starts the trade loop in the BACKGROUND for a loaded setup and returns a stop
// func (cancel + wait for the loop to drain) the caller defers. It builds the safety-gated
// Trader and runs it on a ticker with the monitor-only decider (the safe default: it watches
// real equity, enforces the drawdown gate + kill switch, and reports — but opens no trade
// until a trading policy is wired). When a real-money venue is unconfirmed, or no broker is
// configured, it logs the reason and is a no-op. A run error is logged, never fatal.
func startTradeLoop(configPath string, r tui.SetupResult, ch channel.Channel, confirmLive bool, ks *risk.KillSwitch, logf func(string, ...any)) func() {
	res, err := buildLiveTrader(r, configPath, ch, confirmLive, ks)
	if err != nil {
		logf("trade loop: not started: %v", err)
		return func() {}
	}
	if res.Trader == nil {
		logf("trade loop: %s", res.Reason)
		return func() {}
	}
	var reconciler fillReconciler
	if fillReader, ok := res.Broker.(brokers.FillReader); ok {
		bootNow := time.Now().UTC()
		fillReconciler := ledger.NewFillReconciler(fillReader, res.Store, startOfUTCDay(bootNow))
		// Reconcile-on-boot: seed the ledger's cost basis from the broker before the loop
		// trades only when there is no durable basis to reload. Fail CLOSED — if holdings
		// or persisted reconcile state can't be read, do NOT start (an unseeded ledger
		// would hide a real loss on the next close from the daily-loss breaker).
		coldStart, serr := seedLedgerFromBrokerAt(fillReconciler, res.Broker, bootNow)
		if serr != nil {
			logf("trade loop: not started: %v", serr)
			if res.Store != nil {
				_ = res.Store.Close()
			}
			return func() {}
		}
		if coldStart {
			// First-ever boot with same-day pre-boot activity: the day's realized-loss
			// budget starts fresh here (a bounded cold-start limit). Never silent — tell
			// the operator loudly. Trading is NOT blocked (fills from now on are exact).
			surfaceFirstBootColdStart(context.Background(), ch, logf, bootNow)
		}
		reconciler = fillReconciler
	} else {
		surfaceInactiveFillReconciliation(context.Background(), ch, logf, string(r.Brokers[0].Kind), time.Now().UTC())
	}
	ctx, cancel := context.WithCancel(context.Background())
	ticker := time.NewTicker(tradeLoopInterval)
	done := make(chan struct{})
	// The source feeds the breaker real realized P&L from the ledger store each tick.
	src := newAccountSourceWithAlerts(res.Broker, buildDecider(r, res.Broker, logf), ticker.C, res.Store, reconciler, time.Now, ch, logf)
	go func() {
		defer close(done)
		mode := "paper"
		if res.LiveActive {
			mode = "LIVE (real money)"
		}
		logf("trade loop: running (%s) — watching the account, enforcing drawdown + kill switch", mode)
		if rerr := res.Trader.Run(ctx, src); rerr != nil && ctx.Err() == nil {
			logf("trade loop: stopped with error: %v", rerr)
		}
	}()
	return func() {
		cancel()
		ticker.Stop()
		<-done
		if res.Store != nil {
			_ = res.Store.Close() // release the ledger DB after the loop drains
		}
	}
}

func surfaceInactiveFillReconciliation(ctx context.Context, ch channel.Channel, logf func(string, ...any), venue string, at time.Time) {
	if venue == "" {
		venue = "configured broker"
	}
	text := fmt.Sprintf("Daily-loss breaker INACTIVE: %s does not support fill reconciliation - real losses will not be seen by the breaker.", venue)
	if logf != nil {
		logf("trade loop: %s", text)
	}
	if ch == nil {
		return
	}
	if err := ch.SendAlert(ctx, channel.Alert{Level: channel.AlertCritical, Text: text, Time: at.UTC()}); err != nil && logf != nil {
		logf("trade loop: inactive daily-loss breaker alert failed: %v", err)
	}
}

// surfaceFirstBootColdStart tells the operator LOUDLY that on this first-ever boot the
// daily realized-loss budget starts fresh: any broker trades made earlier today before
// BUCKS started are not counted in today's budget (a bounded first-boot limitation —
// see seedLedgerFromBrokerAt). Trading is not blocked; fills from now on are exact.
func surfaceFirstBootColdStart(ctx context.Context, ch channel.Channel, logf func(string, ...any), at time.Time) {
	text := "First boot: any broker trades made earlier today before BUCKS started are NOT counted in today's realized-loss budget — it starts fresh from this boot. Fills from here on are fully counted."
	if logf != nil {
		logf("trade loop: %s", text)
	}
	if ch == nil {
		return
	}
	if err := ch.SendAlert(ctx, channel.Alert{Level: channel.AlertCritical, Text: text, Time: at.UTC()}); err != nil && logf != nil {
		logf("trade loop: first-boot cold-start alert failed: %v", err)
	}
}
