// Package harness is BUCKS's trade loop: the per-tick brain that turns a strategy/
// analyst signal into an actual (paper) order while obeying every guardrail. It is
// the wiring layer that marries the strategy, the risk engine, the broker, the
// durable kill switch, and the operator channel into the HYBRID-AUTONOMY behavior
// the operator locked (build spec §3.1 / §4.8):
//
//   - Within the per-trade size/risk BAND  -> place automatically (paper default).
//   - Above the band                       -> ask the operator in Telegram and WAIT;
//     place ONLY on an explicit Approved. Deny or timeout -> do NOT place.
//   - Risk-rejected by the engine           -> never reaches the broker; reason logged.
//   - Drawdown breach / already-HALTED switch -> stop trading, halt, alert. No trade
//     ever leaves while the kill switch is HALTED.
//
// DETERMINISM / NO WALL CLOCK: nothing here reads time.Now(). The caller injects a
// clock (now func() time.Time); heartbeat timing, report timing, and every recorded
// timestamp come from it, so the whole loop is reproducible under test. Money is
// orders.Decimal end-to-end — never float64.
//
// CONCURRENCY BOUNDARY: a Trader is driven by ONE goroutine at a time — the loop
// owner calls Tick / Heartbeat / MaybeReport in sequence on a single goroutine
// (Run does exactly this on a ticker). The Trader still guards its own mutable
// state (peak equity, ledger, last-heartbeat/report marks) with a mutex so a
// concurrent reader (e.g. a dashboard calling the Ledger / PeakEquity / IsHalted
// readers, or a -race-exercised test) never sees a torn value. The injected
// dependencies (Engine is stateless; KillSwitch, Broker, MockChannel are each
// internally synchronized) are safe to share. The -race test drives Tick +
// Heartbeat alongside those readers from separate goroutines to prove the boundary
// holds.
package harness

import (
	"context"
	"fmt"
	"sync"
	"time"

	"bucks/internal/brokers"
	"bucks/internal/channel"
	"bucks/internal/orders"
	"bucks/internal/risk"
)

// Decimal is re-exported so trade-loop money stays in the one BUCKS money type.
type Decimal = orders.Decimal

// HybridBandConfig defines the auto-vs-approve boundary — the per-trade band of the
// hybrid-autonomy model. A proposal whose per-trade RISK (qty*|entry-stop|, the
// same quantity the risk engine bounds) AND notional (qty*entry) are both within
// the band is placed automatically; if EITHER exceeds its threshold the trade is
// "above the band" and requires operator approval.
//
// A zero (or negative) threshold means "that dimension does not gate" — e.g. a zero
// MaxAutoNotional means notional never forces an approval and only risk does. If
// BOTH are zero the band is wide open (everything auto) — callers that want a real
// band set at least one.
type HybridBandConfig struct {
	// MaxAutoRiskAmount is the largest per-trade risk (qty*|entry-stop|, in money)
	// that may be placed automatically. Above it -> operator approval. Zero/neg
	// disables the risk dimension of the band.
	MaxAutoRiskAmount Decimal
	// MaxAutoNotional is the largest order notional (qty*entry, in money) that may
	// be placed automatically. Above it -> operator approval. Zero/neg disables the
	// notional dimension of the band.
	MaxAutoNotional Decimal
}

// withinBand reports whether a proposal's risk and notional are both within the
// configured band (and therefore eligible for automatic placement). A disabled
// dimension (zero/neg threshold) never forces an approval. It is decimal-exact.
func (b HybridBandConfig) withinBand(riskAmt, notional Decimal) bool {
	if b.MaxAutoRiskAmount.Sign() > 0 && riskAmt.Cmp(b.MaxAutoRiskAmount) > 0 {
		return false
	}
	if b.MaxAutoNotional.Sign() > 0 && notional.Cmp(b.MaxAutoNotional) > 0 {
		return false
	}
	return true
}

// MarketHours decides whether the market is open at a given instant. The trade loop
// consults it on each heartbeat (and before trading) so off-hours ticks pulse
// liveness without trading. Always24x7 covers crypto.
type MarketHours interface {
	IsOpen(t time.Time) bool
}

// Always24x7 is the crypto market clock: always open.
type Always24x7 struct{}

// IsOpen always returns true.
func (Always24x7) IsOpen(time.Time) bool { return true }

// MarketHoursFunc adapts a plain predicate to MarketHours.
type MarketHoursFunc func(t time.Time) bool

// IsOpen calls the wrapped predicate.
func (f MarketHoursFunc) IsOpen(t time.Time) bool { return f(t) }

// Outcome is what happened to one trade decision on a Tick (for the ledger,
// reports, and tests). It is the machine-readable record of the hybrid-autonomy
// path the proposal took.
type Outcome int

const (
	// OutcomeNoSignal is "the brain said Hold / no proposal this tick".
	OutcomeNoSignal Outcome = iota
	// OutcomeRiskRejected means risk.CheckOrder rejected it — never reached broker.
	OutcomeRiskRejected
	// OutcomeAutoPlaced means it was within the band and placed automatically.
	OutcomeAutoPlaced
	// OutcomeApprovedPlaced means it was above the band, the operator approved, and
	// it was placed.
	OutcomeApprovedPlaced
	// OutcomeDenied means it was above the band and the operator denied it (or the
	// request timed out / errored — fail-safe) — NOT placed.
	OutcomeDenied
	// OutcomeHalted means the kill switch was HALTED, so no trade was attempted.
	OutcomeHalted
	// OutcomeMarketClosed means the market was closed, so no trade was attempted.
	OutcomeMarketClosed
)

// String renders the outcome for logs, reports, and tests.
func (o Outcome) String() string {
	switch o {
	case OutcomeNoSignal:
		return "NoSignal"
	case OutcomeRiskRejected:
		return "RiskRejected"
	case OutcomeAutoPlaced:
		return "AutoPlaced"
	case OutcomeApprovedPlaced:
		return "ApprovedPlaced"
	case OutcomeDenied:
		return "Denied"
	case OutcomeHalted:
		return "Halted"
	case OutcomeMarketClosed:
		return "MarketClosed"
	default:
		return fmt.Sprintf("Outcome(%d)", int(o))
	}
}

// Placed reports whether this outcome resulted in an order reaching the broker.
func (o Outcome) Placed() bool {
	return o == OutcomeAutoPlaced || o == OutcomeApprovedPlaced
}

// TradeDecision is one tick's input: the proposal the brain (strategy or analyst)
// wants to act on, with the provenance Reason for reports. Seq is the monotonic
// intent sequence used to build the deterministic client-order-ID (so a retry on
// the same decision reuses the same id — no duplicate at the broker). A zero-Qty
// proposal (or HasProposal=false) is a Hold: nothing to trade this tick.
type TradeDecision struct {
	HasProposal bool
	Proposal    risk.OrderProposal
	Reason      string
	Seq         uint64
}

// TradeRecord is one entry in the trade ledger: the decision, the path it took,
// the resulting client-order-ID (when placed), and the clock time. It feeds the
// report builder and is what tests assert against.
type TradeRecord struct {
	Time     time.Time
	Symbol   string
	Side     orders.Side
	Qty      Decimal
	EntryPx  Decimal
	Outcome  Outcome
	ClOrdID  string // set when an order was placed (auto or approved)
	Reason   string // strategy/analyst provenance
	RiskInfo string // risk limit that tripped, or band/approval detail
}

// TraderConfig wires a Trader. Strategy/analyst signal generation lives upstream;
// the loop owner feeds the resulting TradeDecision into Tick, which keeps the
// harness focused on the autonomy/risk/channel marriage rather than data plumbing.
type TraderConfig struct {
	StrategyName string // provenance for the deterministic clOrdID + reports
	Engine       *risk.Engine
	Broker       brokers.Broker
	KillSwitch   *risk.KillSwitch
	Channel      channel.Channel
	Band         HybridBandConfig
	Market       MarketHours // nil -> Always24x7 (crypto)

	// Now is the injected clock. REQUIRED — there is no wall-clock fallback in the
	// testable logic; a nil Now is a construction error.
	Now func() time.Time

	// HeartbeatEvery is the minimum interval between heartbeat pulses (driven by the
	// injected clock). Zero -> every tick emits a heartbeat.
	HeartbeatEvery time.Duration
	// ReportEvery is the minimum interval between scheduled reports. Zero -> reports
	// are sent only when MaybeReport is called past-due is trivially true (i.e. each
	// call). Callers typically set a real cadence.
	ReportEvery time.Duration

	// LiveEnabled gates LIVE trading. It defaults false: when false, ALL placement
	// goes to the (paper) Broker regardless. There is no separate live broker in
	// this slice — the flag is the explicit, audited switch the operator flips, and
	// the loop records whether it ran live. Paper is the default by construction.
	LiveEnabled bool

	// ApprovalTimeout bounds how long an above-band approval waits before the
	// fail-safe DENIED applies. The loop derives the approval context deadline from
	// this and the injected clock-independent parent ctx. Zero -> no added deadline
	// (the parent ctx governs); a positive value caps the wait.
	ApprovalTimeout time.Duration
}

// Trader is the hybrid-autonomy trade loop. Construct it with NewTrader; drive it
// with Tick (one decision), Heartbeat (a liveness pulse), and MaybeReport (a
// scheduled report) — or Run to do all three on a ticker.
type Trader struct {
	cfg TraderConfig

	mu            sync.Mutex
	peakEquity    Decimal
	havePeak      bool
	ledger        []TradeRecord
	lastHeartbeat time.Time
	haveHeartbeat bool
	lastReport    time.Time
	haveReport    bool
	halted        bool // latched once we observe/raise a halt (mirrors the switch)
}

// NewTrader validates the config and builds a Trader. A nil Now, Engine, Broker,
// KillSwitch, or Channel is a construction error — the loop never runs half-wired.
func NewTrader(cfg TraderConfig) (*Trader, error) {
	if cfg.Now == nil {
		return nil, fmt.Errorf("harness: Now (injected clock) is required")
	}
	if cfg.Engine == nil {
		return nil, fmt.Errorf("harness: Engine is required")
	}
	if cfg.Broker == nil {
		return nil, fmt.Errorf("harness: Broker is required")
	}
	if cfg.KillSwitch == nil {
		return nil, fmt.Errorf("harness: KillSwitch is required")
	}
	if cfg.Channel == nil {
		return nil, fmt.Errorf("harness: Channel is required")
	}
	if cfg.Market == nil {
		cfg.Market = Always24x7{}
	}
	return &Trader{cfg: cfg}, nil
}

// LiveEnabled reports whether live trading is on (false = paper, the default).
func (t *Trader) LiveEnabled() bool { return t.cfg.LiveEnabled }

// Tick processes ONE trade decision through the full hybrid-autonomy path and
// returns the TradeRecord of what happened. It is the heart of the loop, and the
// AUTHORITATIVE pre-trade drawdown gate: the drawdown breach is evaluated FIRST,
// before any proposal is considered, so a breach on this very tick halts BEFORE a
// trade can be placed (the money-at-risk ordering bug). Step order:
//
//  1. Update peak equity and check the drawdown breach against `equity`. On a
//     breach -> HALT the kill switch, alert, return OutcomeHalted — place NOTHING.
//  2. If the kill switch is already HALTED -> OutcomeHalted, no trade (alert once).
//  3. If the market is closed       -> OutcomeMarketClosed, no trade.
//  4. If there is no proposal       -> OutcomeNoSignal.
//  5. risk.CheckOrder(proposal)     -> rejected => OutcomeRiskRejected, no trade.
//  6. Classify against the band:
//     within -> place automatically (paper).
//     above  -> RequestApproval and WAIT; Approved => place; Denied/timeout => no.
//
// `equity` is the current account equity the caller reads each tick (e.g. from
// broker.Account). It feeds the peak/drawdown gate so the breach is caught on the
// same tick it first occurs — no trade can precede the halt. Heartbeat re-runs the
// same idempotent check for the off-tick liveness path, but Tick is the gate that
// guarantees no placement on the breach tick.
//
// The proposal MUST carry a protective stop (the risk engine enforces it when
// RequireStop is on); Tick does not synthesize one — the brain/strategy supplies
// the stop on entry signals (strategy.Signal.Stop).
func (t *Trader) Tick(ctx context.Context, d TradeDecision, ps risk.PortfolioState, equity Decimal) (TradeRecord, error) {
	now := t.cfg.Now()

	// 1. DRAWDOWN GATE FIRST — update peak + check the breach against current
	//    equity BEFORE any proposal is evaluated. On a breach, halt and place
	//    nothing on this same tick (closes the place-before-halt window).
	breached, peak := t.updatePeakAndCheck(equity)
	if breached {
		if herr := t.haltForDrawdown(ctx, peak, equity); herr != nil {
			return TradeRecord{Time: now, Outcome: OutcomeHalted}, herr
		}
		rec := TradeRecord{Time: now, Outcome: OutcomeHalted, RiskInfo: fmt.Sprintf(
			"drawdown breach: equity %s fell from peak %s past max-drawdown limit", equity.String(), peak.String())}
		t.appendRecord(rec)
		return rec, nil
	}

	// 2. Honor the kill switch — never trade while halted (latched here or raised
	//    elsewhere, e.g. an operator stop or the runaway-order guard).
	if halted, reason := t.cfg.KillSwitch.IsHalted(); halted {
		rec := TradeRecord{Time: now, Outcome: OutcomeHalted, RiskInfo: "kill switch HALTED: " + reason}
		t.recordAndMaybeAlertHalt(ctx, rec, reason)
		return rec, nil
	}

	// 3. Market hours: off-hours, pulse only (the heartbeat path handles liveness);
	//    we do not trade. This is recorded so reports show the skipped tick.
	if !t.cfg.Market.IsOpen(now) {
		rec := TradeRecord{Time: now, Outcome: OutcomeMarketClosed, RiskInfo: "market closed"}
		t.appendRecord(rec)
		return rec, nil
	}

	// 4. No proposal -> Hold.
	if !d.HasProposal || d.Proposal.Qty.Sign() <= 0 {
		rec := TradeRecord{Time: now, Outcome: OutcomeNoSignal, Reason: d.Reason}
		t.appendRecord(rec)
		return rec, nil
	}

	p := d.Proposal
	base := TradeRecord{
		Time:    now,
		Symbol:  p.Symbol,
		Side:    p.Side,
		Qty:     p.Qty,
		EntryPx: p.EntryPx,
		Reason:  d.Reason,
	}

	// 5. Pre-trade risk gate.
	decision := t.cfg.Engine.CheckOrder(p, ps)
	if !decision.Approved {
		rec := base
		rec.Outcome = OutcomeRiskRejected
		rec.RiskInfo = fmt.Sprintf("risk: %s — %s", decision.Limit, decision.Reason)
		t.appendRecord(rec)
		return rec, nil
	}

	// 6. Classify against the hybrid band: compute the per-trade risk amount and
	//    notional in exact decimal, then decide auto vs approve.
	riskAmt, notional, err := tradeMagnitude(p)
	if err != nil {
		rec := base
		rec.Outcome = OutcomeRiskRejected
		rec.RiskInfo = "magnitude compute: " + err.Error()
		t.appendRecord(rec)
		return rec, nil
	}

	if t.cfg.Band.withinBand(riskAmt, notional) {
		return t.placeAuto(ctx, base, p, d.Seq)
	}
	return t.placeAboveBand(ctx, base, p, d.Seq, riskAmt)
}

// placeAuto places a within-band proposal automatically on the (paper) broker. It
// reuses the deterministic clOrdID so a retry never duplicates at the venue.
func (t *Trader) placeAuto(ctx context.Context, base TradeRecord, p risk.OrderProposal, seq uint64) (TradeRecord, error) {
	clOrdID := orders.ClientOrderID(t.cfg.StrategyName, p.Symbol, sideIntent(p.Side), seq)
	if err := t.placeOnBroker(ctx, clOrdID, p); err != nil {
		// A broker placement failure is recorded but is NOT a fake success.
		rec := base
		rec.Outcome = OutcomeRiskRejected // not placed; treated as a non-placement
		rec.RiskInfo = "broker place failed (auto): " + err.Error()
		rec.ClOrdID = clOrdID
		t.appendRecord(rec)
		return rec, err
	}
	rec := base
	rec.Outcome = OutcomeAutoPlaced
	rec.ClOrdID = clOrdID
	rec.RiskInfo = "within band — auto-placed"
	t.appendRecord(rec)
	return rec, nil
}

// placeAboveBand requests operator approval for an above-band proposal and WAITS.
// It places ONLY on an explicit Approved; Denied or a timeout/error fails SAFE to
// NOT placing. The approval context is bounded by ApprovalTimeout (when set).
func (t *Trader) placeAboveBand(ctx context.Context, base TradeRecord, p risk.OrderProposal, seq uint64, riskAmt Decimal) (TradeRecord, error) {
	req := channel.ApprovalRequest{
		Summary: fmt.Sprintf("BUCKS wants to %s %s %s @ %s (stop %s, risk %s) — above your auto-band. Approve?",
			p.Side, p.Qty.String(), p.Symbol, p.EntryPx.String(), p.StopPx.String(), riskAmt.String()),
		Symbol:     p.Symbol,
		Side:       p.Side,
		Qty:        p.Qty,
		EntryPx:    p.EntryPx,
		StopPx:     p.StopPx,
		RiskAmount: riskAmt,
	}

	approvalCtx := ctx
	if t.cfg.ApprovalTimeout > 0 {
		var cancel context.CancelFunc
		approvalCtx, cancel = context.WithTimeout(ctx, t.cfg.ApprovalTimeout)
		defer cancel()
	}

	decision, err := t.cfg.Channel.RequestApproval(approvalCtx, req)
	if !decision.Approved() {
		// Denied OR timeout/error -> fail-safe: do NOT place.
		rec := base
		rec.Outcome = OutcomeDenied
		if err != nil {
			rec.RiskInfo = "above band — not placed (no approval: " + err.Error() + ")"
		} else {
			rec.RiskInfo = "above band — operator DENIED"
		}
		t.appendRecord(rec)
		return rec, nil
	}

	// Approved: place it.
	clOrdID := orders.ClientOrderID(t.cfg.StrategyName, p.Symbol, sideIntent(p.Side), seq)
	if perr := t.placeOnBroker(ctx, clOrdID, p); perr != nil {
		rec := base
		rec.Outcome = OutcomeDenied // approved but not placed (broker error) — not a fill
		rec.RiskInfo = "above band — approved but broker place failed: " + perr.Error()
		rec.ClOrdID = clOrdID
		t.appendRecord(rec)
		return rec, perr
	}
	rec := base
	rec.Outcome = OutcomeApprovedPlaced
	rec.ClOrdID = clOrdID
	rec.RiskInfo = "above band — operator APPROVED, placed"
	t.appendRecord(rec)
	return rec, nil
}

// placeOnBroker submits the order to the (paper) broker. Paper is the default:
// LiveEnabled is recorded but, in this slice, there is one broker (the paper/mock
// broker the loop owner supplied) — the flag is the explicit, audited switch and
// does not silently route to a live venue here. The order is a market order keyed
// by the deterministic clOrdID (idempotent at the venue).
func (t *Trader) placeOnBroker(ctx context.Context, clOrdID string, p risk.OrderProposal) error {
	req := brokers.OrderRequest{
		ClOrdID: clOrdID,
		Symbol:  p.Symbol,
		Side:    p.Side,
		Qty:     p.Qty,
		Kind:    brokers.KindMarket,
	}
	_, err := t.cfg.Broker.PlaceOrder(ctx, req)
	return err
}

// Heartbeat emits a periodic liveness / market-watch pulse driven by the injected
// clock. It also re-runs the DRAWDOWN check: it tracks peak equity across ticks
// and, on a breach, HALTS the kill switch and sends a critical alert. It returns
// true if a heartbeat alert was emitted this call (i.e. the cadence was due). The
// drawdown check runs every call regardless of the heartbeat cadence so a breach
// is never missed between pulses (e.g. an equity move on an off-tick heartbeat).
//
// NOTE: the AUTHORITATIVE pre-trade drawdown gate lives in Tick (step 1), which
// runs the same peak/breach check BEFORE evaluating any proposal — so no trade can
// precede the halt on the breach tick. Heartbeat's check is the off-tick safety
// net for the same mechanism; both share updatePeakAndCheck/haltForDrawdown and the
// halt is idempotent (haltForDrawdown alerts once per halt latch), so running both
// never double-halts or double-alerts.
//
// equity is the current account equity (the loop owner passes the live value, e.g.
// from broker.Account). marketOpen is informational for the alert text.
func (t *Trader) Heartbeat(ctx context.Context, equity Decimal) (emitted bool, err error) {
	now := t.cfg.Now()

	// Drawdown FIRST: update the running peak and check the breach every call.
	breached, peak := t.updatePeakAndCheck(equity)
	if breached {
		if herr := t.haltForDrawdown(ctx, peak, equity); herr != nil {
			return false, herr
		}
	}

	// Heartbeat cadence (clock-driven). Zero interval -> every call emits.
	t.mu.Lock()
	due := !t.haveHeartbeat || t.cfg.HeartbeatEvery <= 0 || !now.Before(t.lastHeartbeat.Add(t.cfg.HeartbeatEvery))
	if due {
		t.lastHeartbeat = now
		t.haveHeartbeat = true
	}
	halted := t.halted
	t.mu.Unlock()

	if !due {
		return false, nil
	}

	open := t.cfg.Market.IsOpen(now)
	level := channel.AlertInfo
	state := "live"
	if halted {
		level = channel.AlertWarn
		state = "HALTED"
	}
	text := fmt.Sprintf("heartbeat: %s | equity %s | market %s", state, equity.String(), marketWord(open))
	if aerr := t.cfg.Channel.SendAlert(ctx, channel.Alert{Level: level, Text: text, Time: now}); aerr != nil {
		return true, aerr
	}
	return true, nil
}

// updatePeakAndCheck updates the running peak equity (the high-water mark across
// ticks) and reports whether the current equity breaches the configured drawdown
// against that peak. Decimal-exact; mutex-guarded.
func (t *Trader) updatePeakAndCheck(equity Decimal) (breached bool, peak Decimal) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.havePeak || equity.Cmp(t.peakEquity) > 0 {
		t.peakEquity = equity
		t.havePeak = true
	}
	peak = t.peakEquity
	breached = t.cfg.Engine.DrawdownBreached(peak, equity)
	return breached, peak
}

// haltForDrawdown halts the kill switch for a drawdown breach (idempotent — an
// already-halted switch stays halted on its first cause) and sends a CRITICAL
// alert. It latches t.halted so subsequent ticks/heartbeats reflect the halt.
func (t *Trader) haltForDrawdown(ctx context.Context, peak, equity Decimal) error {
	reason := fmt.Sprintf("drawdown breach: equity %s fell from peak %s past max-drawdown limit",
		equity.String(), peak.String())
	if err := t.cfg.KillSwitch.Halt(reason, risk.HaltMaxDailyLoss); err != nil {
		// Halting failed durably — surface it; do NOT pretend we halted.
		return fmt.Errorf("harness: halt on drawdown: %w", err)
	}
	t.mu.Lock()
	already := t.halted
	t.halted = true
	t.mu.Unlock()
	if already {
		return nil // alert once per halt latch
	}
	return t.cfg.Channel.SendAlert(ctx, channel.Alert{
		Level: channel.AlertCritical,
		Text:  "HALT — " + reason,
		Time:  t.cfg.Now(),
	})
}

// recordAndMaybeAlertHalt records a halted-tick outcome and sends a single warn
// alert the first time the loop observes the halt (so a halted switch raised
// elsewhere — e.g. the runaway-order guard or an operator Halt — still notifies).
func (t *Trader) recordAndMaybeAlertHalt(ctx context.Context, rec TradeRecord, reason string) {
	t.appendRecord(rec)
	t.mu.Lock()
	already := t.halted
	t.halted = true
	t.mu.Unlock()
	if already {
		return
	}
	_ = t.cfg.Channel.SendAlert(ctx, channel.Alert{
		Level: channel.AlertCritical,
		Text:  "trading halted: " + reason,
		Time:  rec.Time,
	})
}

// appendRecord adds a record to the ledger under the mutex.
func (t *Trader) appendRecord(rec TradeRecord) {
	t.mu.Lock()
	t.ledger = append(t.ledger, rec)
	t.mu.Unlock()
}

// Ledger returns a copy of the trade ledger (every Tick outcome), in order.
func (t *Trader) Ledger() []TradeRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]TradeRecord, len(t.ledger))
	copy(out, t.ledger)
	return out
}

// PeakEquity returns the running high-water-mark equity (zero value if no tick has
// set it yet) and whether it has been set.
func (t *Trader) PeakEquity() (Decimal, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.peakEquity, t.havePeak
}

// IsHalted reports the loop's latched halt view (true once a halt is observed or
// raised). The authoritative durable flag is the KillSwitch; this mirrors it for
// the loop's own fast checks and the dashboard.
func (t *Trader) IsHalted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.halted
}

// tradeMagnitude computes the per-trade risk amount (qty*|entry-stop|) and the
// order notional (qty*entry) in exact decimal. When the proposal has no stop the
// risk amount is zero (the band's notional dimension still gates).
func tradeMagnitude(p risk.OrderProposal) (riskAmt, notional Decimal, err error) {
	notional, err = p.Qty.Mul(p.EntryPx)
	if err != nil {
		return orders.ZeroDecimal, orders.ZeroDecimal, fmt.Errorf("notional: %w", err)
	}
	if p.StopPx.Sign() <= 0 {
		return orders.ZeroDecimal, notional, nil
	}
	diff, err := p.EntryPx.Sub(p.StopPx)
	if err != nil {
		return orders.ZeroDecimal, orders.ZeroDecimal, fmt.Errorf("stop distance: %w", err)
	}
	riskAmt, err = p.Qty.Mul(diff.Abs())
	if err != nil {
		return orders.ZeroDecimal, orders.ZeroDecimal, fmt.Errorf("risk amount: %w", err)
	}
	return riskAmt, notional, nil
}

// sideIntent maps a side to the intent token used in the deterministic clOrdID, so
// a buy and a sell on the same symbol/seq never collide.
func sideIntent(s orders.Side) string {
	if s == orders.SideSell {
		return "sell"
	}
	return "buy"
}

// marketWord renders the market-open state for alert text.
func marketWord(open bool) string {
	if open {
		return "open"
	}
	return "closed"
}
