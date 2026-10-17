package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bucks/internal/channel"
	"bucks/internal/harness"
	"bucks/internal/orders"
	"bucks/internal/risk"
	"bucks/internal/tui"
)

// TestBuildLiveTraderPaperRuns proves a PAPER setup builds a running Trader (no real-money
// venue), and the broker is pointed at the PAPER API — it can never reach the live endpoint.
func TestBuildLiveTraderPaperRuns(t *testing.T) {
	var paperHit, liveHit bool
	paperSrv := acctServer(&paperHit, nil)
	defer paperSrv.Close()
	liveSrv := acctServer(&liveHit, nil)
	defer liveSrv.Close()
	defer swapAlpacaURLs(paperSrv.URL, liveSrv.URL, paperSrv.URL)()

	res, err := buildLiveTrader(validSetupResult(t), filepath.Join(t.TempDir(), "bucks.yaml"),
		channel.NewMockChannel(), false, nil)
	if err != nil {
		t.Fatalf("buildLiveTrader: %v", err)
	}
	if res.Trader == nil {
		t.Fatalf("paper setup must build a running trader; reason: %q", res.Reason)
	}
	if res.LiveActive {
		t.Error("paper setup must NOT be live-active")
	}
	if res.Trader.LiveEnabled() {
		t.Error("paper trader must have LiveEnabled=false")
	}
	if _, err := res.Broker.Account(context.Background()); err != nil {
		t.Fatalf("broker account: %v", err)
	}
	if !paperHit || liveHit {
		t.Errorf("paper trader hit the wrong venue (paperHit=%v liveHit=%v)", paperHit, liveHit)
	}
}

// TestBuildLiveTraderRefusesRealMoneyWithoutLegacyConfirmation proves the
// refusal does not depend on the old confirmation value.
func TestBuildLiveTraderRefusesRealMoneyWithoutLegacyConfirmation(t *testing.T) {
	var paperHit, liveHit bool
	paperSrv := acctServer(&paperHit, nil)
	defer paperSrv.Close()
	liveSrv := acctServer(&liveHit, nil)
	defer liveSrv.Close()
	defer swapAlpacaURLs(paperSrv.URL, liveSrv.URL, paperSrv.URL)()

	res, err := buildLiveTrader(liveArmedSetupResult(t), filepath.Join(t.TempDir(), "bucks.yaml"),
		channel.NewMockChannel(), false, nil)
	if err != nil {
		t.Fatalf("buildLiveTrader: %v", err)
	}
	if res.Trader != nil {
		t.Fatal("SAFETY VIOLATION: a real-money setup started the loop")
	}
	if res.LiveActive {
		t.Fatal("SAFETY VIOLATION: refused setup reported live-active")
	}
	if !strings.Contains(strings.ToLower(res.Reason), "live") {
		t.Errorf("reason should explain the real-money refusal, got %q", res.Reason)
	}
	if liveHit {
		t.Fatal("SAFETY VIOLATION: an unconfirmed live setup reached the live venue")
	}
}

// TestBuildLiveTraderRefusesToArmRealMoney proves that even an explicit session
// confirmation cannot construct a real-money loop while the broker contract has
// neither venue-side protective stops nor an exit path.
func TestBuildLiveTraderRefusesToArmRealMoney(t *testing.T) {
	res, err := buildLiveTrader(liveArmedSetupResult(t), filepath.Join(t.TempDir(), "bucks.yaml"),
		channel.NewMockChannel(), true, nil)
	if err != nil {
		t.Fatalf("buildLiveTrader: %v", err)
	}
	if res.Trader != nil || res.Broker != nil {
		t.Fatalf("SAFETY VIOLATION: real-money setup armed despite missing venue protection and exit path: %+v", res)
	}
	if res.LiveActive {
		t.Fatal("SAFETY VIOLATION: refused real-money setup reported live-active")
	}
	reason := strings.ToLower(res.Reason)
	for _, want := range []string{"protective stop", "sizing", "exit path", "re-enable", "bracket", "oco"} {
		if !strings.Contains(reason, want) {
			t.Errorf("refusal reason %q does not contain %q", res.Reason, want)
		}
	}
}

// TestLiveLoopHonorsSharedKillSwitchHalt proves an operator /halt trips the shared
// kill switch, and the paper loop's trader — built with that same switch — must
// then refuse to trade. A tick carrying an entry proposal returns OutcomeHalted and places
// nothing. (IsHalted reads in-memory state, so this only holds when the daemon's command
// context and the loop share one switch — which buildLiveTrader now requires.)
func TestLiveLoopHonorsSharedKillSwitchHalt(t *testing.T) {
	dir := t.TempDir()
	ks, err := risk.Open(filepath.Join(dir, "killswitch.json"))
	if err != nil {
		t.Fatalf("ks: %v", err)
	}

	var paperHit, liveHit bool
	paperSrv := acctServer(&paperHit, nil)
	defer paperSrv.Close()
	liveSrv := acctServer(&liveHit, nil)
	defer liveSrv.Close()
	defer swapAlpacaURLs(paperSrv.URL, liveSrv.URL, paperSrv.URL)()

	res, err := buildLiveTrader(validSetupResult(t), filepath.Join(dir, "bucks.yaml"), channel.NewMockChannel(), false, ks)
	if err != nil {
		t.Fatalf("buildLiveTrader: %v", err)
	}
	if res.Trader == nil {
		t.Fatalf("trader nil: %s", res.Reason)
	}

	// Operator /halt trips the SHARED switch.
	if err := ks.Halt("operator /halt", risk.HaltManual); err != nil {
		t.Fatalf("halt: %v", err)
	}

	// A tick carrying an entry proposal must NOT place — the loop honors the halt.
	decision := harness.TradeDecision{
		HasProposal: true,
		Proposal: risk.OrderProposal{
			Symbol: "BTC", Side: orders.SideBuy,
			Qty: dec(t, "1"), EntryPx: dec(t, "100"), StopPx: dec(t, "99"),
			AccountEquity: dec(t, "100000"),
		},
		Reason: "entry while halted", Seq: 1,
	}
	ps := risk.PortfolioState{Equity: dec(t, "100000"), Cash: dec(t, "100000"), OpenPositionCount: -1}
	rec, err := res.Trader.Tick(context.Background(), decision, ps, dec(t, "100000"))
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if rec.Outcome != harness.OutcomeHalted {
		t.Errorf("a tick while HALTED must be OutcomeHalted (no trade), got %s", rec.Outcome)
	}
}

func TestStartTradeLoopAlertsWhenRealMoneyIsDisabled(t *testing.T) {
	r := validSetupResult(t)
	r.Brokers = []tui.BrokerCreds{{Kind: tui.BrokerAlpacaLive, Key: "live-key", Secret: "live-secret"}}
	ch := channel.NewMockChannel()
	var logs []string
	stop := startTradeLoop(filepath.Join(t.TempDir(), "bucks.yaml"), r, ch, true, nil, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	stop()

	alerts := ch.Alerts()
	if len(alerts) != 1 || alerts[0].Level != channel.AlertCritical {
		t.Fatalf("real-money refusal must send one critical operator alert, got %+v", alerts)
	}
	want := []string{"protective stop", "exit path"}
	for _, phrase := range want {
		if !strings.Contains(strings.ToLower(alerts[0].Text), phrase) {
			t.Errorf("alert %q does not contain %q", alerts[0].Text, phrase)
		}
	}
	if len(logs) != 1 || !strings.Contains(strings.ToLower(logs[0]), "refusing to arm") {
		t.Fatalf("real-money refusal must be logged once, got %v", logs)
	}
}

// The first-boot cold-start notice must reach the operator LOUDLY (critical alert +
// log), never a silent debug line.
func TestSurfaceFirstBootColdStart_AlertsOperatorLoudly(t *testing.T) {
	ch := channel.NewMockChannel()
	var logs []string
	surfaceFirstBootColdStart(context.Background(), ch, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}, time.Date(2026, 7, 4, 15, 0, 0, 0, time.UTC))

	alerts := ch.Alerts()
	if len(alerts) == 0 {
		t.Fatalf("first-boot cold start must surface a loud operator alert")
	}
	if alerts[0].Level != channel.AlertCritical ||
		!strings.Contains(alerts[0].Text, "budget") ||
		!strings.Contains(alerts[0].Text, "starts fresh") {
		t.Fatalf("unexpected cold-start alert: %+v", alerts[0])
	}
	var sawLog bool
	for _, line := range logs {
		if strings.Contains(line, "starts fresh") {
			sawLog = true
		}
	}
	if !sawLog {
		t.Fatalf("cold-start notice must also log; logs=%v", logs)
	}
}
