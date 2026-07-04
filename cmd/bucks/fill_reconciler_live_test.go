package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bucks/internal/brokers"
	"bucks/internal/brokers/mock"
	"bucks/internal/channel"
	"bucks/internal/harness"
	"bucks/internal/ledger"
	"bucks/internal/memory"
	"bucks/internal/orders"
	"bucks/internal/risk"
)

type scriptedFillReader struct {
	fills []brokers.Fill
	err   error
}

func (s *scriptedFillReader) FillsSince(ctx context.Context, after time.Time) ([]brokers.Fill, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.err != nil {
		return nil, s.err
	}
	var out []brokers.Fill
	for _, f := range s.fills {
		if f.At.After(after) {
			out = append(out, f)
		}
	}
	return out, nil
}

func nTicks(n int) <-chan time.Time {
	c := make(chan time.Time, n)
	for i := 0; i < n; i++ {
		c <- time.Time{}
	}
	close(c)
	return c
}

func TestAccountSourceReconcilesOutOfBandFillBeforeDailyLossCheck(t *testing.T) {
	store := tempStore(t)
	b := mock.New()
	b.SetAccount(brokers.Account{
		Equity:      dec(t, "1000"),
		Cash:        dec(t, "1000"),
		BuyingPower: dec(t, "1000"),
	})
	seedCursor := time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)
	closeAt := seedCursor.Add(time.Minute)
	reader := &scriptedFillReader{fills: []brokers.Fill{{
		ID:     "manual-stop-fill-1",
		Symbol: "AAPL",
		Side:   orders.SideSell,
		Qty:    dec(t, "10"),
		Px:     dec(t, "90"),
		At:     closeAt,
	}}}
	reconciler := ledger.NewFillReconciler(reader, store, seedCursor)
	reconciler.Seed("AAPL", dec(t, "10"), dec(t, "100"))

	ks, err := risk.Open(t.TempDir() + "/killswitch.json")
	if err != nil {
		t.Fatalf("kill switch: %v", err)
	}
	trader, err := harness.NewTrader(harness.TraderConfig{
		StrategyName: "reconcile-source-test",
		Engine: risk.NewEngine(risk.Config{
			MaxRiskPerTradePct: dec(t, "0.01"),
			MaxDailyLossPct:    dec(t, "0.03"),
			RequireStop:        true,
		}),
		Broker:     b,
		KillSwitch: ks,
		Channel:    channel.NewMockChannel(),
		Band:       harness.HybridBandConfig{},
		Market:     harness.Always24x7{},
		Now:        func() time.Time { return closeAt },
	})
	if err != nil {
		t.Fatalf("trader: %v", err)
	}

	var snapRealized orders.Decimal
	decider := DeciderFunc(func(_ context.Context, snap AccountSnapshot) harness.TradeDecision {
		snapRealized = snap.Portfolio.RealizedPnLToday
		return harness.TradeDecision{
			HasProposal: true,
			Proposal: risk.OrderProposal{
				Symbol: "AAPL", Side: orders.SideBuy,
				Qty: dec(t, "1"), EntryPx: dec(t, "100"), StopPx: dec(t, "99"),
				AccountEquity: snap.Equity,
			},
			Reason: "would trade if daily loss were blind",
			Seq:    1,
		}
	})
	src := newAccountSource(b, decider, oneTick(), store, reconciler, func() time.Time { return closeAt })
	if err := trader.Run(context.Background(), src); err != nil {
		t.Fatalf("run: %v", err)
	}

	realized, err := store.RealizedPnLSince(startOfUTCDay(closeAt))
	if err != nil {
		t.Fatalf("realized: %v", err)
	}
	if realized.Cmp(dec(t, "-100")) != 0 {
		t.Fatalf("realized today = %s, want -100", realized.String())
	}
	if snapRealized.Cmp(dec(t, "-100")) != 0 {
		t.Fatalf("source fed realized = %s, want -100 before risk check", snapRealized.String())
	}
	records := trader.Ledger()
	if len(records) != 1 {
		t.Fatalf("ledger records = %d, want 1: %+v", len(records), records)
	}
	if records[0].Outcome != harness.OutcomeHalted || !strings.Contains(records[0].RiskInfo, "Daily-loss limit hit") {
		t.Fatalf("daily-loss breaker did not fire on reconciled out-of-band loss: %+v", records[0])
	}
	if placed := b.Placed(); len(placed) != 0 {
		t.Fatalf("daily-loss breaker fired too late; placed orders = %v", placed)
	}
}

func TestAccountSourceReloadsDurableBasisAndCatchesOfflineCloseBeforeBoot(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "memory.sqlite")
	store, err := memory.Open(storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedStartedAt := time.Now().UTC()
	closeAt := seedStartedAt.Add(time.Minute)
	bootAt := closeAt.Add(time.Minute)

	previousBroker := mock.New()
	previousBroker.SetPosition(brokers.Position{
		Symbol:     "AAPL",
		Qty:        dec(t, "10"),
		AvgEntryPx: dec(t, "100"),
	})
	previousRun := ledger.NewFillReconciler(previousBroker, store, startOfUTCDay(seedStartedAt))
	if _, err := seedLedgerFromBrokerAt(previousRun, previousBroker, seedStartedAt); err != nil {
		t.Fatalf("previous seed: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close previous store: %v", err)
	}

	reopened, err := memory.Open(storePath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	b := mock.New()
	b.SetAccount(brokers.Account{
		Equity:      dec(t, "1000"),
		Cash:        dec(t, "1000"),
		BuyingPower: dec(t, "1000"),
	})
	b.SetFills([]brokers.Fill{{
		ID:     "offline-stop-fill-1",
		Symbol: "AAPL",
		Side:   orders.SideSell,
		Qty:    dec(t, "10"),
		Px:     dec(t, "90"),
		At:     closeAt,
	}})
	restarted := ledger.NewFillReconciler(b, reopened, bootAt)
	if _, err := seedLedgerFromBroker(restarted, b); err != nil {
		t.Fatalf("restart seed/load: %v", err)
	}

	ks, err := risk.Open(t.TempDir() + "/killswitch.json")
	if err != nil {
		t.Fatalf("kill switch: %v", err)
	}
	trader, err := harness.NewTrader(harness.TraderConfig{
		StrategyName: "offline-close-restart-test",
		Engine: risk.NewEngine(risk.Config{
			MaxRiskPerTradePct: dec(t, "0.01"),
			MaxDailyLossPct:    dec(t, "0.03"),
			RequireStop:        true,
		}),
		Broker:     b,
		KillSwitch: ks,
		Channel:    channel.NewMockChannel(),
		Band:       harness.HybridBandConfig{},
		Market:     harness.Always24x7{},
		Now:        func() time.Time { return bootAt },
	})
	if err != nil {
		t.Fatalf("trader: %v", err)
	}

	var snapRealized orders.Decimal
	decider := DeciderFunc(func(_ context.Context, snap AccountSnapshot) harness.TradeDecision {
		snapRealized = snap.Portfolio.RealizedPnLToday
		return harness.TradeDecision{
			HasProposal: true,
			Proposal: risk.OrderProposal{
				Symbol: "AAPL", Side: orders.SideBuy,
				Qty: dec(t, "1"), EntryPx: dec(t, "100"), StopPx: dec(t, "99"),
				AccountEquity: snap.Equity,
			},
			Reason: "would trade if restart replay missed the offline close",
			Seq:    1,
		}
	})
	src := newAccountSource(b, decider, oneTick(), reopened, restarted, func() time.Time { return bootAt })
	if err := trader.Run(context.Background(), src); err != nil {
		t.Fatalf("run: %v", err)
	}

	realized, err := reopened.RealizedPnLSince(startOfUTCDay(bootAt))
	if err != nil {
		t.Fatalf("realized: %v", err)
	}
	if realized.Cmp(dec(t, "-100")) != 0 {
		t.Fatalf("realized after restart = %s, want -100 from offline close", realized.String())
	}
	if snapRealized.Cmp(dec(t, "-100")) != 0 {
		t.Fatalf("source fed realized = %s, want -100 before risk check", snapRealized.String())
	}
	records := trader.Ledger()
	if len(records) != 1 {
		t.Fatalf("ledger records = %d, want 1: %+v", len(records), records)
	}
	if records[0].Outcome != harness.OutcomeHalted || !strings.Contains(records[0].RiskInfo, "Daily-loss limit hit") {
		t.Fatalf("daily-loss breaker did not fire on offline close replay: %+v", records[0])
	}
	if placed := b.Placed(); len(placed) != 0 {
		t.Fatalf("daily-loss breaker fired too late; placed orders = %v", placed)
	}

	restartedAgain := ledger.NewFillReconciler(b, reopened, bootAt.Add(time.Minute))
	if _, err := seedLedgerFromBroker(restartedAgain, b); err != nil {
		t.Fatalf("second restart load: %v", err)
	}
	if err := restartedAgain.Reconcile(context.Background()); err != nil {
		t.Fatalf("second restart reconcile: %v", err)
	}
	again, err := reopened.RealizedPnLSince(startOfUTCDay(bootAt))
	if err != nil {
		t.Fatalf("realized after second restart: %v", err)
	}
	if again.Cmp(realized) != 0 {
		t.Fatalf("realized double-counted across normal restart: got %s, before %s", again.String(), realized.String())
	}
}

type failingReconciler struct {
	err   error
	calls int
}

func (f *failingReconciler) Reconcile(context.Context) error {
	f.calls++
	return f.err
}

func TestAccountSourceSkipsTickWhenFillReconcileFails(t *testing.T) {
	b := mock.New()
	b.SetAccount(brokers.Account{
		Equity:      dec(t, "1000"),
		Cash:        dec(t, "1000"),
		BuyingPower: dec(t, "1000"),
	})
	reconciler := &failingReconciler{err: errors.New("activity stream down")}
	var deciderCalls int
	src := newAccountSource(b, DeciderFunc(func(context.Context, AccountSnapshot) harness.TradeDecision {
		deciderCalls++
		return harness.TradeDecision{}
	}), oneTick(), nil, reconciler, time.Now)

	if _, ok := src.Next(context.Background()); ok {
		t.Fatalf("source yielded a tick after reconcile failed; it must skip rather than feed false zero P&L")
	}
	if reconciler.calls != 1 {
		t.Fatalf("reconcile calls = %d, want 1", reconciler.calls)
	}
	if deciderCalls != 0 {
		t.Fatalf("decider was called %d times after reconcile failure; want 0", deciderCalls)
	}
}

func TestAccountSourceAlertsWhenFillReconcileKeepsFailing(t *testing.T) {
	b := mock.New()
	b.SetAccount(brokers.Account{
		Equity:      dec(t, "1000"),
		Cash:        dec(t, "1000"),
		BuyingPower: dec(t, "1000"),
	})
	ch := channel.NewMockChannel()
	reconciler := &failingReconciler{err: errors.New("activity stream down")}
	var deciderCalls int
	now := time.Date(2026, 7, 4, 15, 0, 0, 0, time.UTC)
	src := newAccountSourceWithAlerts(b, DeciderFunc(func(context.Context, AccountSnapshot) harness.TradeDecision {
		deciderCalls++
		return harness.TradeDecision{}
	}), nTicks(3), nil, reconciler, func() time.Time { return now }, ch, nil)

	if _, ok := src.Next(context.Background()); ok {
		t.Fatalf("source yielded a tick after persistent reconcile failures; it must skip rather than feed false zero P&L")
	}
	if reconciler.calls != 3 {
		t.Fatalf("reconcile calls = %d, want 3", reconciler.calls)
	}
	if deciderCalls != 0 {
		t.Fatalf("decider was called %d times after reconcile failure; want 0", deciderCalls)
	}
	alerts := ch.Alerts()
	if len(alerts) == 0 {
		t.Fatalf("persistent reconcile failure must send a loud operator alert")
	}
	if alerts[0].Level != channel.AlertCritical ||
		!strings.Contains(alerts[0].Text, "Fill reconciliation failing") ||
		!strings.Contains(alerts[0].Text, "daily-loss breaker") {
		t.Fatalf("unexpected reconcile failure alert: %+v", alerts[0])
	}
}
