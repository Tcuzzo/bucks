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
	"bucks/internal/orders"
	"bucks/internal/playbook"
	"bucks/internal/risk"
)

type stateReadFailBroker struct {
	brokers.Broker
	accountErr   error
	positionsErr error
}

func (b stateReadFailBroker) Account(ctx context.Context) (brokers.Account, error) {
	if b.accountErr != nil {
		return brokers.Account{}, b.accountErr
	}
	return b.Broker.Account(ctx)
}

func (b stateReadFailBroker) Positions(ctx context.Context) ([]brokers.Position, error) {
	if b.positionsErr != nil {
		return nil, b.positionsErr
	}
	return b.Broker.Positions(ctx)
}

func dec(t *testing.T, s string) orders.Decimal {
	t.Helper()
	d, err := orders.ParseDecimal(s)
	if err != nil {
		t.Fatalf("decimal %q: %v", s, err)
	}
	return d
}

// oneTick returns a pace channel that yields exactly ONE poll then stops the loop (buffered
// value + close), so a test drives the trade loop deterministically with no wall-clock sleep.
func oneTick() <-chan time.Time {
	c := make(chan time.Time, 1)
	c <- time.Time{}
	close(c)
	return c
}

// TestAccountSourceReadsRealEquityAndHolds proves the Source feeds the loop the broker's REAL
// equity (so the drawdown/risk gates see real money) and, with the safe default decider,
// returns a Hold (the loop monitors but invents no trade).
func TestAccountSourceReadsRealEquityAndHolds(t *testing.T) {
	b := mock.New()
	b.SetAccount(brokers.Account{Equity: dec(t, "12345"), Cash: dec(t, "12345"), BuyingPower: dec(t, "12345")})

	src := newAccountSource(b, monitorOnlyDecider(), oneTick(), nil, nil, nil)
	in, ok := src.Next(context.Background())
	if !ok {
		t.Fatal("source should yield one tick")
	}
	if in.Equity.Cmp(dec(t, "12345")) != 0 {
		t.Errorf("source equity = %s, want the broker's real 12345", in.Equity)
	}
	if in.Decision.HasProposal {
		t.Error("monitor-only decider must Hold (no proposal)")
	}
}

func TestAccountSourceFailsClosedAndAlertsOnBrokerStateReadFailure(t *testing.T) {
	tests := []struct {
		name         string
		accountErr   error
		positionsErr error
		wantAlert    string
	}{
		{name: "account auth failure", accountErr: errors.New("401 invalid signature"), wantAlert: "account state read failing"},
		{name: "positions unavailable", positionsErr: errors.New("positions API unavailable"), wantAlert: "positions state read failing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := mock.New()
			base.SetAccount(brokers.Account{Equity: dec(t, "1000"), Cash: dec(t, "1000")})
			b := stateReadFailBroker{Broker: base, accountErr: tt.accountErr, positionsErr: tt.positionsErr}
			ch := channel.NewMockChannel()
			var deciderCalls int
			src := newAccountSourceWithAlerts(b, DeciderFunc(func(context.Context, AccountSnapshot) harness.TradeDecision {
				deciderCalls++
				return harness.TradeDecision{}
			}), oneTick(), nil, nil, time.Now, ch, nil)

			if _, ok := src.Next(context.Background()); ok {
				t.Fatal("source yielded a tick from unknown broker state")
			}
			if deciderCalls != 0 {
				t.Fatalf("decider called %d times with unknown broker state", deciderCalls)
			}
			alerts := ch.Alerts()
			if len(alerts) != 1 || alerts[0].Level != channel.AlertCritical || !strings.Contains(strings.ToLower(alerts[0].Text), tt.wantAlert) {
				t.Fatalf("broker state failure was not surfaced loudly: %+v", alerts)
			}
		})
	}
}

// TestLiveLoopPlacesInjectedDecision is the END-TO-END proof that the wired loop actually
// PLACES a real order: a funded mock broker + a real harness.Trader, driven by the account
// Source with a decider that returns one in-band entry, places exactly one order on the broker.
// This proves saved-keys -> broker -> trade loop -> real placement works (here on the mock, so
// no real money moves), closing the P0 "no production path ever places an order".
func TestLiveLoopPlacesInjectedDecision(t *testing.T) {
	b := mock.New()
	b.SetAccount(brokers.Account{Equity: dec(t, "100000"), Cash: dec(t, "100000"), BuyingPower: dec(t, "100000")})

	pb, err := playbook.BuildPlaybook(map[string]string{
		playbook.KeyRiskTolerance: "moderate",
		playbook.KeyCapital:       "100000",
		playbook.KeyStyle:         "swing",
		playbook.KeyMaxDrawdown:   "0.20",
	})
	if err != nil {
		t.Fatalf("playbook: %v", err)
	}
	ks, err := risk.Open(filepath.Join(t.TempDir(), "killswitch.json"))
	if err != nil {
		t.Fatalf("kill switch: %v", err)
	}
	fixed := time.Date(2026, 6, 19, 15, 0, 0, 0, time.UTC)
	trader, err := harness.NewTrader(harness.TraderConfig{
		StrategyName: "live-source-test",
		Engine:       risk.NewEngine(pb.ToRiskConfig()),
		Broker:       b,
		KillSwitch:   ks,
		Channel:      channel.NewMockChannel(),
		Band:         harness.HybridBandConfig{}, // wide open -> within-band entry auto-places
		Market:       harness.Always24x7{},
		Now:          func() time.Time { return fixed },
		LiveEnabled:  false, // paper/mock — the placement PATH is identical; no real money here
	})
	if err != nil {
		t.Fatalf("trader: %v", err)
	}

	// A decider that proposes one tiny in-band BUY (with a protective stop) on its tick.
	decider := DeciderFunc(func(_ context.Context, snap AccountSnapshot) harness.TradeDecision {
		return harness.TradeDecision{
			HasProposal: true,
			Proposal: risk.OrderProposal{
				Symbol: "BTC", Side: orders.SideBuy,
				Qty: dec(t, "1"), EntryPx: dec(t, "100"), StopPx: dec(t, "99"),
				AccountEquity: snap.Equity,
			},
			Reason: "test entry",
			Seq:    1,
		}
	})

	src := newAccountSource(b, decider, oneTick(), nil, nil, nil)
	if err := trader.Run(context.Background(), src); err != nil {
		t.Fatalf("trader.Run: %v", err)
	}

	placed := b.Placed()
	if len(placed) != 1 {
		t.Fatalf("live loop should have placed exactly 1 order, got %d: %v", len(placed), placed)
	}
	ledger := trader.Ledger()
	if len(ledger) != 1 || !ledger[0].Outcome.Placed() {
		t.Errorf("ledger should record one placed trade, got %+v", ledger)
	}
}
