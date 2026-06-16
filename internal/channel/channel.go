// Package channel is BUCKS's operator channel: the one typed boundary the trader
// uses to talk to its owner. Three things cross it — an ALERT (one-way liveness /
// breach / heartbeat notice), an APPROVAL REQUEST (the hybrid-band "this trade is
// above your auto-band, may I place it?" round trip, §3.1/§4.8 of the build spec),
// and a REPORT (scheduled P&L / position / trade-rationale summary).
//
// The boundary is an interface so the trade loop (package harness) depends on
// Channel, never on Telegram directly. Two implementations live here:
//
//   - MockChannel — an in-memory, deterministic Channel for tests: scripted
//     approve/deny/timeout decisions, and it records every alert/report sent so a
//     test can assert exactly what the trader said. NO network, ever.
//   - TelegramChannel — the live Bot API implementation (its OWN bot token, never
//     shared — the 409 tap-drop lesson, spec §6). Its real network calls are
//     compiled ONLY under the `telegram_live` build tag (telegram_live.go), so the
//     default test suite can NEVER make a live call.
//
// FAIL-SAFE APPROVAL: RequestApproval treats a timeout (or any transport error) as
// DENIED, never as approved. A trade above the band is placed ONLY on an explicit
// Approved — silence is a no. This binds the operator-authority / approval-gate
// law (destructive/above-band moves are gated; the operator approves first).
//
// No wall clock lives in the testable logic here: callers inject the deadline via
// the context and the trader injects its clock. No secrets are embedded in this
// file or anywhere in the package.
package channel

import (
	"context"
	"time"

	"bucks/internal/orders"
)

// Decimal is re-exported so channel/report money stays in the one BUCKS money type
// (orders.Decimal — never float64).
type Decimal = orders.Decimal

// Decision is the operator's answer to an approval request. The zero value is
// DecisionDenied so that any unset / defaulted decision fails SAFE (no trade).
type Decision int

const (
	// DecisionDenied is the fail-safe default: do NOT place the trade. A timeout,
	// a transport error, or an explicit "no" all land here. Silence is a no.
	DecisionDenied Decision = iota
	// DecisionApproved means the operator explicitly approved the above-band trade.
	DecisionApproved
)

// String renders the decision for logs and tests.
func (d Decision) String() string {
	switch d {
	case DecisionApproved:
		return "Approved"
	case DecisionDenied:
		return "Denied"
	default:
		return "Denied"
	}
}

// Approved reports whether this decision authorizes the trade. Only an explicit
// DecisionApproved does; everything else (including the zero value) is a no.
func (d Decision) Approved() bool { return d == DecisionApproved }

// AlertLevel tags the urgency/category of a one-way alert so the operator (and a
// quiet-hours filter downstream) can triage. It does not change channel behavior
// here; it is metadata carried to the operator surface.
type AlertLevel int

const (
	// AlertInfo is routine liveness / status (e.g. a heartbeat pulse).
	AlertInfo AlertLevel = iota
	// AlertWarn is a notable but non-emergency condition.
	AlertWarn
	// AlertCritical is a breach / halt the operator must see (e.g. kill switch).
	AlertCritical
)

// String renders the alert level.
func (l AlertLevel) String() string {
	switch l {
	case AlertWarn:
		return "Warn"
	case AlertCritical:
		return "Critical"
	default:
		return "Info"
	}
}

// Alert is a one-way message to the operator (no answer expected). Time is the
// trader's injected clock value at send (never time.Now() inside testable logic).
type Alert struct {
	Level AlertLevel
	Text  string
	Time  time.Time
}

// ApprovalRequest is the above-band trade put to the operator for a yes/no. It
// carries the plain-English summary the operator sees plus the machine-readable
// proposal fields so the channel surface can render a clear "Approve / Deny".
//
// Symbol/Side/Qty/EntryPx/StopPx mirror the risk.OrderProposal that passed the
// risk gate; RiskAmount is the per-trade risk (qty*|entry-stop|) that exceeded the
// auto-band; Summary is the human-readable line ("BUCKS wants to BUY 10 AAPL...").
type ApprovalRequest struct {
	Summary    string
	Symbol     string
	Side       orders.Side
	Qty        Decimal
	EntryPx    Decimal
	StopPx     Decimal
	RiskAmount Decimal
}

// Position is one open position line in a report (signed qty: + long / - short),
// marked at MarkPx, with its unrealized P&L (exact decimal).
type Position struct {
	Symbol       string
	Qty          Decimal
	MarkPx       Decimal
	UnrealizedPL Decimal
}

// TradeRationale is one recent decision's provenance for the report: what the
// trader did and why (the strategy/analyst reason), at the trader's clock time.
type TradeRationale struct {
	Symbol  string
	Side    orders.Side
	Qty     Decimal
	Reason  string
	Auto    bool // true = placed automatically within the band; false = operator-approved
	Time    time.Time
	Skipped bool   // true = NOT placed (risk-rejected, denied, or timed out)
	SkipWhy string // populated when Skipped (e.g. "risk: PerTradeRisk", "operator denied")
}

// Report is the scheduled P&L / position / trade-rationale summary. RealizedPL and
// UnrealizedPL are exact decimal; Equity is the account equity at report time;
// Positions and Rationales are deterministically ordered by the builder.
type Report struct {
	GeneratedAt  time.Time
	Equity       Decimal
	RealizedPL   Decimal
	UnrealizedPL Decimal
	Positions    []Position
	Rationales   []TradeRationale
}

// Channel is the operator-channel boundary. Every method takes a context for
// cancellation/timeout; the trade loop depends on this interface, not on any
// concrete transport.
type Channel interface {
	// SendAlert delivers a one-way alert to the operator (no answer expected).
	SendAlert(ctx context.Context, a Alert) error

	// RequestApproval asks the operator to approve an above-band trade and BLOCKS
	// until an answer, the context deadline, or a transport failure. It MUST fail
	// SAFE: a timeout or any error returns DecisionDenied (never Approved). The
	// returned error reports the transport/timeout cause for logging; the Decision
	// is authoritative for the trade decision regardless (Denied on any error).
	RequestApproval(ctx context.Context, r ApprovalRequest) (Decision, error)

	// SendReport delivers a scheduled report to the operator.
	SendReport(ctx context.Context, r Report) error
}
