package harness

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bucks/internal/brokers"
	"bucks/internal/brokers/mock"
	"bucks/internal/channel"
	"bucks/internal/orders"
	"bucks/internal/risk"
)

// ---- test helpers -------------------------------------------------------------

func dec(t *testing.T, s string) Decimal {
	t.Helper()
	d, err := orders.ParseDecimal(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return d
}

// fixedClock is a controllable injected clock: tests advance it explicitly so the
// loop's timing (heartbeat/report cadence, timestamps) is fully deterministic.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fixedClock {
	return &fixedClock{t: time.Date(2026, 6, 15, 14, 30, 0, 0, time.UTC)}
}
func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fixedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newKillSwitch builds a durable kill switch in a temp dir (no real operator path).
func newKillSwitch(t *testing.T, clk *fixedClock) *risk.KillSwitch {
	t.Helper()
	path := filepath.Join(t.TempDir(), "killswitch.json")
	ks, err := risk.Open(path, risk.WithClock(clk.now))
	if err != nil {
		t.Fatalf("open kill switch: %v", err)
	}
	return ks
}

// newEngine builds a risk engine with a generous config so risk only rejects the
// cases a test deliberately constructs, plus the drawdown limit under test.
func newEngine(t *testing.T, maxDrawdownPct string) *risk.Engine {
	t.Helper()
	cfg := risk.Config{
		MaxRiskPerTradePct:  dec(t, "0.02"), // 2% — generous so band, not risk, gates
		MaxConcentrationPct: dec(t, "1"),    // 100% — don't trip concentration
		MaxOpenPositions:    100,
		RequireStop:         true,
	}
	if maxDrawdownPct != "" {
		cfg.MaxDrawdownPct = dec(t, maxDrawdownPct)
	}
	return risk.NewEngine(cfg)
}

// harnessFixture bundles a wired Trader with its mocks for assertions.
type harnessFixture struct {
	trader *Trader
	broker *mock.MockBroker
	ch     *channel.MockChannel
	ks     *risk.KillSwitch
	clk    *fixedClock
}

// newFixture wires a Trader with a band of: auto if risk <= $50 AND notional <=
// $5000; above that -> approval. Market is 24/7 (crypto) unless overridden.
func newFixture(t *testing.T, opts func(*TraderConfig)) *harnessFixture {
	t.Helper()
	clk := newClock()
	br := mock.New()
	br.SetAccount(brokers.Account{Cash: dec(t, "100000"), Equity: dec(t, "100000")})
	ch := channel.NewMockChannel()
	ks := newKillSwitch(t, clk)

	cfg := TraderConfig{
		StrategyName: "momentum",
		Engine:       newEngine(t, ""),
		Broker:       br,
		KillSwitch:   ks,
		Channel:      ch,
		Band: HybridBandConfig{
			MaxAutoRiskAmount: dec(t, "50"),
			MaxAutoNotional:   dec(t, "5000"),
		},
		Now: clk.now,
	}
	if opts != nil {
		opts(&cfg)
	}
	tr, err := NewTrader(cfg)
	if err != nil {
		t.Fatalf("new trader: %v", err)
	}
	return &harnessFixture{trader: tr, broker: br, ch: ch, ks: ks, clk: clk}
}

// emptyPortfolio is a flat book with the given equity.
func emptyPortfolio(t *testing.T, equity string) risk.PortfolioState {
	t.Helper()
	return risk.PortfolioState{
		Equity:            dec(t, equity),
		Positions:         map[string]risk.HeldPosition{},
		OpenPositionCount: -1,
	}
}

// longProposal builds a long entry with a protective stop below entry.
func longProposal(t *testing.T, sym, qty, entry, stop, equity string) risk.OrderProposal {
	t.Helper()
	return risk.OrderProposal{
		Symbol:        sym,
		Side:          orders.SideBuy,
		Qty:           dec(t, qty),
		EntryPx:       dec(t, entry),
		StopPx:        dec(t, stop),
		AccountEquity: dec(t, equity),
	}
}

// ---- WITHIN BAND -> AUTO ------------------------------------------------------

// TestTick_WithinBand_AutoPlaced proves a within-band, risk-approved proposal is
// placed AUTOMATICALLY on the broker with NO approval requested.
func TestTick_WithinBand_AutoPlaced(t *testing.T) {
	f := newFixture(t, nil)
	// qty 10 @ entry 100, stop 99 -> risk = 10*1 = $10 (<=50), notional = $1000 (<=5000): within band.
	p := longProposal(t, "AAPL", "10", "100", "99", "100000")
	ps := emptyPortfolio(t, "100000")

	rec, err := f.trader.Tick(context.Background(), TradeDecision{HasProposal: true, Proposal: p, Reason: "MA cross up", Seq: 1}, ps, dec(t, "100000"))
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if rec.Outcome != OutcomeAutoPlaced {
		t.Fatalf("want AutoPlaced, got %s (%s)", rec.Outcome, rec.RiskInfo)
	}
	if f.ch.ApprovalCount() != 0 {
		t.Fatalf("within-band must NOT request approval, got %d requests", f.ch.ApprovalCount())
	}
	placed := f.broker.Placed()
	if len(placed) != 1 || placed[0] != rec.ClOrdID {
		t.Fatalf("want exactly the order placed; placed=%v rec.ClOrdID=%s", placed, rec.ClOrdID)
	}
	if !orders.ValidClientOrderID(rec.ClOrdID) {
		t.Fatalf("clOrdID not the deterministic shape: %q", rec.ClOrdID)
	}
}

// ---- ABOVE BAND -> APPROVE/DENY/TIMEOUT --------------------------------------

// TestTick_AboveBand_Approved proves an above-band proposal REQUESTS approval and,
// on Approved, is placed.
func TestTick_AboveBand_Approved(t *testing.T) {
	f := newFixture(t, nil)
	f.ch.ScriptApprove()
	// qty 100 @ 100, stop 99 -> risk = $100 (>50): above band.
	p := longProposal(t, "AAPL", "100", "100", "99", "100000")
	ps := emptyPortfolio(t, "100000")

	rec, err := f.trader.Tick(context.Background(), TradeDecision{HasProposal: true, Proposal: p, Reason: "breakout", Seq: 2}, ps, dec(t, "100000"))
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if f.ch.ApprovalCount() != 1 {
		t.Fatalf("above-band must request approval exactly once, got %d", f.ch.ApprovalCount())
	}
	if rec.Outcome != OutcomeApprovedPlaced {
		t.Fatalf("want ApprovedPlaced, got %s (%s)", rec.Outcome, rec.RiskInfo)
	}
	if len(f.broker.Placed()) != 1 {
		t.Fatalf("approved trade must be placed exactly once, placed=%v", f.broker.Placed())
	}
	// The approval request carried the right symbol + risk.
	req := f.ch.ApprovalRequests()[0]
	if req.Symbol != "AAPL" || req.RiskAmount.Cmp(dec(t, "100")) != 0 {
		t.Fatalf("approval request payload wrong: %+v", req)
	}
}

// TestTick_AboveBand_Denied proves an above-band proposal that is DENIED is NOT
// placed (fail-safe).
func TestTick_AboveBand_Denied(t *testing.T) {
	f := newFixture(t, nil)
	f.ch.ScriptDeny()
	p := longProposal(t, "AAPL", "100", "100", "99", "100000")
	ps := emptyPortfolio(t, "100000")

	rec, err := f.trader.Tick(context.Background(), TradeDecision{HasProposal: true, Proposal: p, Seq: 3}, ps, dec(t, "100000"))
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if rec.Outcome != OutcomeDenied {
		t.Fatalf("want Denied, got %s", rec.Outcome)
	}
	if len(f.broker.Placed()) != 0 {
		t.Fatalf("DENIED trade must NOT be placed, placed=%v", f.broker.Placed())
	}
	if f.ch.ApprovalCount() != 1 {
		t.Fatalf("want 1 approval request, got %d", f.ch.ApprovalCount())
	}
}

// TestTick_AboveBand_Timeout proves an above-band proposal whose approval TIMES OUT
// is NOT placed (fail-safe — silence is a no).
func TestTick_AboveBand_Timeout(t *testing.T) {
	f := newFixture(t, nil)
	f.ch.ScriptTimeout()
	p := longProposal(t, "AAPL", "100", "100", "99", "100000")
	ps := emptyPortfolio(t, "100000")

	rec, err := f.trader.Tick(context.Background(), TradeDecision{HasProposal: true, Proposal: p, Seq: 4}, ps, dec(t, "100000"))
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if rec.Outcome != OutcomeDenied {
		t.Fatalf("timeout must fail-safe to Denied outcome, got %s", rec.Outcome)
	}
	if len(f.broker.Placed()) != 0 {
		t.Fatalf("TIMED-OUT trade must NOT be placed, placed=%v", f.broker.Placed())
	}
}

// TestTick_AboveBand_NotionalDimension proves the notional dimension of the band
// also forces approval even when per-trade risk is tiny.
func TestTick_AboveBand_NotionalDimension(t *testing.T) {
	f := newFixture(t, nil)
	f.ch.ScriptApprove()
	// qty 60 @ 100, stop 99.9 -> risk = 60*0.1 = $6 (<=50) BUT notional = $6000 (>5000).
	p := longProposal(t, "AAPL", "60", "100", "99.9", "100000")
	ps := emptyPortfolio(t, "100000")

	rec, err := f.trader.Tick(context.Background(), TradeDecision{HasProposal: true, Proposal: p, Seq: 5}, ps, dec(t, "100000"))
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if f.ch.ApprovalCount() != 1 {
		t.Fatalf("notional above band must request approval, got %d", f.ch.ApprovalCount())
	}
	if rec.Outcome != OutcomeApprovedPlaced {
		t.Fatalf("want ApprovedPlaced, got %s", rec.Outcome)
	}
}

// ---- RISK REJECTED -> NOT PLACED ----------------------------------------------

// TestTick_RiskRejected_NotPlaced proves a proposal the risk engine rejects never
// reaches the broker and records the tripped limit — no approval requested either.
func TestTick_RiskRejected_NotPlaced(t *testing.T) {
	f := newFixture(t, nil)
	// Missing stop -> RequireStop rejects (LimitMissingStop). StopPx zero.
	p := risk.OrderProposal{
		Symbol:        "AAPL",
		Side:          orders.SideBuy,
		Qty:           dec(t, "10"),
		EntryPx:       dec(t, "100"),
		StopPx:        orders.ZeroDecimal, // no stop
		AccountEquity: dec(t, "100000"),
	}
	ps := emptyPortfolio(t, "100000")

	rec, err := f.trader.Tick(context.Background(), TradeDecision{HasProposal: true, Proposal: p, Seq: 6}, ps, dec(t, "100000"))
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if rec.Outcome != OutcomeRiskRejected {
		t.Fatalf("want RiskRejected, got %s", rec.Outcome)
	}
	if len(f.broker.Placed()) != 0 {
		t.Fatalf("risk-rejected trade must NOT be placed, placed=%v", f.broker.Placed())
	}
	if f.ch.ApprovalCount() != 0 {
		t.Fatalf("risk-rejected trade must NOT request approval, got %d", f.ch.ApprovalCount())
	}
	if rec.RiskInfo == "" {
		t.Fatalf("risk rejection reason must be recorded")
	}
}

func TestTick_DailyLossBreachBlocksTradeAlertsOnceAndClearsNextDay(t *testing.T) {
	f := newFixture(t, func(c *TraderConfig) {
		c.Engine = risk.NewEngine(risk.Config{
			MaxRiskPerTradePct:  dec(t, "0.02"),
			MaxDailyLossPct:     dec(t, "0.03"),
			MaxConcentrationPct: dec(t, "1"),
			MaxOpenPositions:    100,
			RequireStop:         true,
		})
	})
	ctx := context.Background()
	p := longProposal(t, "AAPL", "1", "100", "99", "1000")
	breached := emptyPortfolio(t, "1000")
	breached.RealizedPnLToday = dec(t, "-30")

	rec, err := f.trader.Tick(ctx, TradeDecision{HasProposal: true, Proposal: p, Reason: "entry after loss", Seq: 12}, breached, dec(t, "1000"))
	if err != nil {
		t.Fatalf("breach tick: %v", err)
	}
	if rec.Outcome != OutcomeHalted {
		t.Fatalf("daily-loss breach must halt this tick, got %s (%s)", rec.Outcome, rec.RiskInfo)
	}
	if len(f.broker.Placed()) != 0 {
		t.Fatalf("daily-loss breach must block placement, placed=%v", f.broker.Placed())
	}
	if halted(f.ks) {
		t.Fatalf("daily-loss breaker must not trip the durable kill switch")
	}
	alerts := f.ch.Alerts()
	if len(alerts) != 1 || alerts[0].Level != channel.AlertCritical ||
		!strings.Contains(alerts[0].Text, "Daily-loss limit hit") {
		t.Fatalf("daily-loss breach must send one loud alert, alerts=%+v", alerts)
	}

	rec, err = f.trader.Tick(ctx, TradeDecision{HasProposal: true, Proposal: p, Reason: "still breached", Seq: 13}, breached, dec(t, "1000"))
	if err != nil {
		t.Fatalf("second breach tick: %v", err)
	}
	if rec.Outcome != OutcomeHalted {
		t.Fatalf("daily-loss breach must keep blocking while breached, got %s", rec.Outcome)
	}
	if got := len(f.ch.Alerts()); got != 1 {
		t.Fatalf("daily-loss breach must alert only on transition, got %d alerts", got)
	}

	f.clk.advance(24 * time.Hour)
	cleared := emptyPortfolio(t, "1000")
	rec, err = f.trader.Tick(ctx, TradeDecision{HasProposal: true, Proposal: p, Reason: "new UTC day", Seq: 14}, cleared, dec(t, "1000"))
	if err != nil {
		t.Fatalf("next-day tick: %v", err)
	}
	if rec.Outcome != OutcomeAutoPlaced {
		t.Fatalf("daily-loss breaker should clear when today's realized resets, got %s (%s)", rec.Outcome, rec.RiskInfo)
	}
	if len(f.broker.Placed()) != 1 {
		t.Fatalf("next-day cleared breaker should allow placement, placed=%v", f.broker.Placed())
	}
	if halted(f.ks) {
		t.Fatalf("daily-loss auto-clear must not require kill-switch re-arm")
	}
}

// ---- DRAWDOWN HALT ------------------------------------------------------------

// TestHeartbeat_DrawdownBreach_Halts proves the heartbeat path tracks peak equity,
// detects a drawdown breach, HALTS the kill switch, alerts, and that subsequent
// ticks refuse to trade.
func TestHeartbeat_DrawdownBreach_Halts(t *testing.T) {
	f := newFixture(t, func(c *TraderConfig) {
		c.Engine = newEngine(t, "0.20") // 20% max drawdown
	})
	ctx := context.Background()

	// Tick 1: equity 100k sets the peak; no breach.
	if _, err := f.trader.Heartbeat(ctx, dec(t, "100000")); err != nil {
		t.Fatalf("hb1: %v", err)
	}
	if halted(f.ks) {
		t.Fatalf("no halt expected at peak")
	}
	// Tick 2: equity 79k -> 21% drop from peak 100k -> breach.
	f.clk.advance(time.Minute)
	if _, err := f.trader.Heartbeat(ctx, dec(t, "79000")); err != nil {
		t.Fatalf("hb2: %v", err)
	}
	if !halted(f.ks) {
		t.Fatalf("drawdown breach must HALT the kill switch")
	}
	if !f.trader.IsHalted() {
		t.Fatalf("trader must reflect the halt")
	}
	// A critical alert was sent for the halt.
	if !hasCritical(f.ch.Alerts()) {
		t.Fatalf("drawdown halt must send a critical alert; alerts=%+v", f.ch.Alerts())
	}

	// Now a trade tick must refuse (Halted), placing nothing.
	p := longProposal(t, "AAPL", "10", "100", "99", "79000")
	rec, err := f.trader.Tick(ctx, TradeDecision{HasProposal: true, Proposal: p, Seq: 7}, emptyPortfolio(t, "79000"), dec(t, "79000"))
	if err != nil {
		t.Fatalf("post-halt tick: %v", err)
	}
	if rec.Outcome != OutcomeHalted {
		t.Fatalf("post-halt tick must be Halted, got %s", rec.Outcome)
	}
	if len(f.broker.Placed()) != 0 {
		t.Fatalf("nothing may be placed after a halt, placed=%v", f.broker.Placed())
	}
}

// TestTick_DrawdownBreachSameTick_NoTrade proves the money-at-risk ordering fix:
// when the equity delivered to a Tick first breaches the drawdown AND a valid,
// in-band, risk-approved proposal arrives on that SAME tick, the drawdown gate
// fires FIRST — the kill switch HALTS and NOTHING is placed on the broker. Against
// the old ordering (Tick placed before Heartbeat ran the drawdown check) this trade
// would have been auto-placed BEFORE the halt; with the gate inside Tick it cannot.
func TestTick_DrawdownBreachSameTick_NoTrade(t *testing.T) {
	f := newFixture(t, func(c *TraderConfig) {
		c.Engine = newEngine(t, "0.20") // 20% max drawdown
	})
	ctx := context.Background()

	// Tick 1: equity 100k sets the peak with a Hold (no proposal) — no breach yet.
	if _, err := f.trader.Tick(ctx, TradeDecision{HasProposal: false}, emptyPortfolio(t, "100000"), dec(t, "100000")); err != nil {
		t.Fatalf("seed-peak tick: %v", err)
	}
	if halted(f.ks) {
		t.Fatalf("no halt expected at the peak-setting tick")
	}

	// Tick 2: equity 79k (21% drop from peak 100k -> breach) delivered ALONGSIDE a
	// valid, within-band, risk-approved proposal (qty 10 @ 100, stop 99 -> risk $10,
	// notional $1000 — both inside the $50/$5000 band). The drawdown gate must win.
	f.clk.advance(time.Minute)
	p := longProposal(t, "AAPL", "10", "100", "99", "79000")
	rec, err := f.trader.Tick(ctx, TradeDecision{HasProposal: true, Proposal: p, Reason: "MA cross up", Seq: 11}, emptyPortfolio(t, "79000"), dec(t, "79000"))
	if err != nil {
		t.Fatalf("breach tick: %v", err)
	}
	if rec.Outcome != OutcomeHalted {
		t.Fatalf("same-tick breach must yield Halted (gate before placement), got %s (%s)", rec.Outcome, rec.RiskInfo)
	}
	if len(f.broker.Placed()) != 0 {
		t.Fatalf("NOTHING may be placed on the breach tick — drawdown gate must precede placement; placed=%v", f.broker.Placed())
	}
	if f.ch.ApprovalCount() != 0 {
		t.Fatalf("no approval may be requested on the breach tick, got %d", f.ch.ApprovalCount())
	}
	if !halted(f.ks) {
		t.Fatalf("same-tick drawdown breach must HALT the kill switch")
	}
	if !f.trader.IsHalted() {
		t.Fatalf("trader must reflect the halt after the breach tick")
	}
	if !hasCritical(f.ch.Alerts()) {
		t.Fatalf("same-tick drawdown halt must send a critical alert; alerts=%+v", f.ch.Alerts())
	}
}

// TestTick_AlreadyHalted_NoTrades proves a switch HALTED before any tick blocks all
// trading from the first tick (honors an externally-raised halt).
func TestTick_AlreadyHalted_NoTrades(t *testing.T) {
	f := newFixture(t, nil)
	if err := f.ks.Halt("operator stop", risk.HaltManual); err != nil {
		t.Fatalf("pre-halt: %v", err)
	}
	p := longProposal(t, "AAPL", "10", "100", "99", "100000")
	rec, err := f.trader.Tick(context.Background(), TradeDecision{HasProposal: true, Proposal: p, Seq: 8}, emptyPortfolio(t, "100000"), dec(t, "100000"))
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if rec.Outcome != OutcomeHalted {
		t.Fatalf("already-halted must yield Halted, got %s", rec.Outcome)
	}
	if len(f.broker.Placed()) != 0 {
		t.Fatalf("no trades while halted, placed=%v", f.broker.Placed())
	}
	if f.ch.ApprovalCount() != 0 {
		t.Fatalf("no approval requested while halted, got %d", f.ch.ApprovalCount())
	}
}

// ---- HEARTBEAT CADENCE --------------------------------------------------------

// TestHeartbeat_Cadence proves heartbeats fire on the injected clock at the
// configured interval (not every call when an interval is set).
func TestHeartbeat_Cadence(t *testing.T) {
	f := newFixture(t, func(c *TraderConfig) { c.HeartbeatEvery = 5 * time.Minute })
	ctx := context.Background()

	// First call always emits (no prior heartbeat).
	emitted, err := f.trader.Heartbeat(ctx, dec(t, "100000"))
	if err != nil || !emitted {
		t.Fatalf("first heartbeat must emit; emitted=%v err=%v", emitted, err)
	}
	// Only 1 minute later -> NOT due.
	f.clk.advance(time.Minute)
	emitted, _ = f.trader.Heartbeat(ctx, dec(t, "100000"))
	if emitted {
		t.Fatalf("heartbeat must NOT emit before the interval elapses")
	}
	// Past the interval -> due again.
	f.clk.advance(5 * time.Minute)
	emitted, _ = f.trader.Heartbeat(ctx, dec(t, "100000"))
	if !emitted {
		t.Fatalf("heartbeat must emit once the interval elapses")
	}
	// Exactly the two emitted heartbeats are recorded as info alerts.
	if n := countInfoAlerts(f.ch.Alerts()); n != 2 {
		t.Fatalf("want 2 heartbeat alerts, got %d", n)
	}
}

// ---- REPORT -------------------------------------------------------------------

// TestReport_BuildAndSend proves the report is built with correct Decimal P&L /
// positions and sent via the channel, deterministically.
func TestReport_BuildAndSend(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	// Seed broker positions: long 10 AAPL @ 100, short 5 MSFT @ 200.
	f.broker.SetAccount(brokers.Account{Cash: dec(t, "50000"), Equity: dec(t, "102500")})
	f.broker.SetPosition(brokers.Position{Symbol: "AAPL", Qty: dec(t, "10"), AvgEntryPx: dec(t, "100")})
	f.broker.SetPosition(brokers.Position{Symbol: "MSFT", Qty: dec(t, "-5"), AvgEntryPx: dec(t, "200")})

	// Drive one auto-placed trade so a rationale appears.
	p := longProposal(t, "AAPL", "10", "100", "99", "102500")
	if _, err := f.trader.Tick(ctx, TradeDecision{HasProposal: true, Proposal: p, Reason: "MA cross", Seq: 9}, emptyPortfolio(t, "102500"), dec(t, "102500")); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// Marks: AAPL up to 110 (+10*10 = +$100), MSFT down to 190 (short gains: (190-200)*-5 = +$50).
	marks := map[string]Decimal{"AAPL": dec(t, "110"), "MSFT": dec(t, "190")}
	sent, err := f.trader.MaybeReport(ctx, dec(t, "12.50"), marks)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if !sent {
		t.Fatalf("report must be sent")
	}
	if f.ch.ReportCount() != 1 {
		t.Fatalf("want 1 report sent, got %d", f.ch.ReportCount())
	}
	rep := f.ch.Reports()[0]
	if rep.Equity.Cmp(dec(t, "102500")) != 0 {
		t.Fatalf("report equity wrong: %s", rep.Equity.String())
	}
	if rep.RealizedPL.Cmp(dec(t, "12.50")) != 0 {
		t.Fatalf("report realized PnL wrong: %s", rep.RealizedPL.String())
	}
	// Unrealized = +100 (AAPL) + 50 (MSFT short) = +150, exact decimal.
	if rep.UnrealizedPL.Cmp(dec(t, "150")) != 0 {
		t.Fatalf("report unrealized PnL wrong: want 150, got %s", rep.UnrealizedPL.String())
	}
	// Positions deterministically sorted by symbol (AAPL before MSFT).
	if len(rep.Positions) != 2 || rep.Positions[0].Symbol != "AAPL" || rep.Positions[1].Symbol != "MSFT" {
		t.Fatalf("positions not deterministically sorted: %+v", rep.Positions)
	}
	if rep.Positions[0].UnrealizedPL.Cmp(dec(t, "100")) != 0 {
		t.Fatalf("AAPL uPnL wrong: %s", rep.Positions[0].UnrealizedPL.String())
	}
	if rep.Positions[1].UnrealizedPL.Cmp(dec(t, "50")) != 0 {
		t.Fatalf("MSFT short uPnL wrong: %s", rep.Positions[1].UnrealizedPL.String())
	}
	// The placed trade shows as a rationale.
	if len(rep.Rationales) != 1 || rep.Rationales[0].Symbol != "AAPL" || !rep.Rationales[0].Auto {
		t.Fatalf("rationale wrong: %+v", rep.Rationales)
	}
}

// TestReport_Cadence proves reports respect the injected-clock schedule.
func TestReport_Cadence(t *testing.T) {
	f := newFixture(t, func(c *TraderConfig) { c.ReportEvery = time.Hour })
	ctx := context.Background()
	marks := map[string]Decimal{}

	sent, _ := f.trader.MaybeReport(ctx, orders.ZeroDecimal, marks)
	if !sent {
		t.Fatalf("first report must send")
	}
	f.clk.advance(30 * time.Minute)
	sent, _ = f.trader.MaybeReport(ctx, orders.ZeroDecimal, marks)
	if sent {
		t.Fatalf("report must NOT send before the interval")
	}
	f.clk.advance(time.Hour)
	sent, _ = f.trader.MaybeReport(ctx, orders.ZeroDecimal, marks)
	if !sent {
		t.Fatalf("report must send after the interval")
	}
	if f.ch.ReportCount() != 2 {
		t.Fatalf("want 2 reports, got %d", f.ch.ReportCount())
	}
}

// ---- MARKET HOURS -------------------------------------------------------------

// TestTick_MarketClosed_NoTrade proves an off-hours tick does not trade.
func TestTick_MarketClosed_NoTrade(t *testing.T) {
	closed := MarketHoursFunc(func(time.Time) bool { return false })
	f := newFixture(t, func(c *TraderConfig) { c.Market = closed })
	p := longProposal(t, "AAPL", "10", "100", "99", "100000")
	rec, err := f.trader.Tick(context.Background(), TradeDecision{HasProposal: true, Proposal: p, Seq: 10}, emptyPortfolio(t, "100000"), dec(t, "100000"))
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if rec.Outcome != OutcomeMarketClosed {
		t.Fatalf("closed market must not trade, got %s", rec.Outcome)
	}
	if len(f.broker.Placed()) != 0 {
		t.Fatalf("nothing placed when market closed, placed=%v", f.broker.Placed())
	}
}

// ---- NO SIGNAL ----------------------------------------------------------------

// TestTick_NoSignal proves a Hold (no proposal) records NoSignal and trades nothing.
func TestTick_NoSignal(t *testing.T) {
	f := newFixture(t, nil)
	rec, err := f.trader.Tick(context.Background(), TradeDecision{HasProposal: false, Reason: "hold"}, emptyPortfolio(t, "100000"), dec(t, "100000"))
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if rec.Outcome != OutcomeNoSignal {
		t.Fatalf("want NoSignal, got %s", rec.Outcome)
	}
	if len(f.broker.Placed()) != 0 || f.ch.ApprovalCount() != 0 {
		t.Fatalf("no-signal must do nothing")
	}
}

// ---- LIVE FLAG DEFAULT --------------------------------------------------------

// TestLiveEnabled_DefaultsFalse proves paper is the default: LiveEnabled is false
// unless explicitly flipped.
func TestLiveEnabled_DefaultsFalse(t *testing.T) {
	f := newFixture(t, nil)
	if f.trader.LiveEnabled() {
		t.Fatalf("live trading must default OFF (paper)")
	}
	f2 := newFixture(t, func(c *TraderConfig) { c.LiveEnabled = true })
	if !f2.trader.LiveEnabled() {
		t.Fatalf("explicit LiveEnabled flip must register")
	}
}

// ---- CONSTRUCTION GUARDS ------------------------------------------------------

// TestNewTrader_RequiresDeps proves a half-wired loop never constructs.
func TestNewTrader_RequiresDeps(t *testing.T) {
	clk := newClock()
	base := func() TraderConfig {
		return TraderConfig{
			Engine:     newEngine(t, ""),
			Broker:     mock.New(),
			KillSwitch: newKillSwitch(t, clk),
			Channel:    channel.NewMockChannel(),
			Now:        clk.now,
		}
	}
	// Missing clock.
	c := base()
	c.Now = nil
	if _, err := NewTrader(c); err == nil {
		t.Fatalf("missing Now must error")
	}
	// Missing engine.
	c = base()
	c.Engine = nil
	if _, err := NewTrader(c); err == nil {
		t.Fatalf("missing Engine must error")
	}
	// Missing channel.
	c = base()
	c.Channel = nil
	if _, err := NewTrader(c); err == nil {
		t.Fatalf("missing Channel must error")
	}
}

// ---- NO LIVE CALL IN DEFAULT SUITE --------------------------------------------

// TestDefaultSuiteUsesMockChannelOnly proves the harness default suite drives the
// in-memory MockChannel — no live transport. The MockChannel makes no network call
// (proven in the channel package's nolive tests); here we assert the trade loop is
// wired to it, so a normal `go test ./...` never pages a real operator.
func TestDefaultSuiteUsesMockChannelOnly(t *testing.T) {
	f := newFixture(t, nil)
	if _, ok := f.trader.cfg.Channel.(*channel.MockChannel); !ok {
		t.Fatalf("default suite must use the in-memory MockChannel, got %T", f.trader.cfg.Channel)
	}
}

// ---- helpers for assertions ---------------------------------------------------

// halted reports the kill switch's boolean halt state (dropping the reason).
func halted(ks *risk.KillSwitch) bool {
	h, _ := ks.IsHalted()
	return h
}

func hasCritical(alerts []channel.Alert) bool {
	for _, a := range alerts {
		if a.Level == channel.AlertCritical {
			return true
		}
	}
	return false
}

func countInfoAlerts(alerts []channel.Alert) int {
	n := 0
	for _, a := range alerts {
		if a.Level == channel.AlertInfo {
			n++
		}
	}
	return n
}
