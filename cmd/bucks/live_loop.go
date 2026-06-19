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
	"bucks/internal/playbook"
	"bucks/internal/risk"
	"bucks/internal/tui"
)

// liveTraderResult is what buildLiveTrader produced. When Trader is nil the loop was NOT
// started (Reason says why, in plain English) — the SAFE outcome for a real-money venue that
// the owner has not explicitly confirmed this session. LiveActive is true only when a
// real-money venue is actually armed (so the caller can shout it in logs / the dashboard).
type liveTraderResult struct {
	Trader     *harness.Trader
	Broker     brokers.Broker
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
		ApprovalTimeout: 5 * time.Minute,
	})
	if err != nil {
		return liveTraderResult{}, fmt.Errorf("live loop: construct trader: %w", err)
	}

	return liveTraderResult{Trader: trader, Broker: broker, LiveActive: liveActive}, nil
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
	ctx, cancel := context.WithCancel(context.Background())
	ticker := time.NewTicker(tradeLoopInterval)
	done := make(chan struct{})
	src := newAccountSource(res.Broker, buildDecider(r, res.Broker, logf), ticker.C)
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
	}
}
