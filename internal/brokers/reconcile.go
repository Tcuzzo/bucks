package brokers

import (
	"context"
	"fmt"

	"bucks/internal/orders"
)

// ResolvedStatus is how reconcile classified a previously in-flight order after
// asking the broker for its true state.
type ResolvedStatus int

const (
	// ResolvedOpen means the broker still shows the order as working
	// (New/PartiallyFilled) — it is genuinely live and must be re-armed/tracked.
	ResolvedOpen ResolvedStatus = iota
	// ResolvedFilled means the order actually filled at the broker while we were
	// down (the headline crash-recovery case: an intent with no terminal journal
	// event that the broker reports as Filled).
	ResolvedFilled
	// ResolvedCanceled means the broker reports the order canceled.
	ResolvedCanceled
	// ResolvedRejected means the broker reports the order rejected.
	ResolvedRejected
	// ResolvedMissing means the broker has NO record of the order: the intent
	// was journaled but the send never reached the broker (crash between the
	// durable INTENT and the network send). Safe — no position was taken.
	ResolvedMissing
)

// String renders the resolved status.
func (r ResolvedStatus) String() string {
	switch r {
	case ResolvedFilled:
		return "Filled"
	case ResolvedCanceled:
		return "Canceled"
	case ResolvedRejected:
		return "Rejected"
	case ResolvedMissing:
		return "Missing"
	default:
		return "Open"
	}
}

// ResolvedOrder is one in-flight journal order resolved against the broker's
// truth. BrokerOrder is the broker's view (zero-valued when Status==Missing).
type ResolvedOrder struct {
	ClOrdID string
	Symbol  string
	Status  ResolvedStatus
	Broker  BrokerOrder
}

// PositionDiscrepancy is a mismatch between the position the journal's terminal
// fills imply and the position the broker actually holds. Either side may be
// absent (zero qty) — a position the journal doesn't know about, or one the
// broker has closed that the journal still implies.
type PositionDiscrepancy struct {
	Symbol     string
	JournalQty orders.Decimal // signed qty implied by journal terminal fills
	BrokerQty  orders.Decimal // signed qty the broker reports
}

// ReconcileResult is the outcome of ReconcileOnBoot. Clean is true iff there are
// zero position discrepancies between the journal and the broker. Resolved holds
// every previously in-flight order with its broker-confirmed fate; Discrepancies
// holds every symbol where journal-implied and broker qty disagree.
type ReconcileResult struct {
	Resolved      []ResolvedOrder
	Discrepancies []PositionDiscrepancy
	Clean         bool
}

// ReconcileOnBoot replays the WAL at journalPath, resolves every in-flight order
// against the broker (the source of truth for fills), and diffs broker positions
// against the positions implied by the journal's terminal fills.
//
// Contract:
//   - The broker is the source of truth for fills and positions; the journal is
//     the source of truth for INTENT.
//   - An order with an INTENT but no terminal journal event is "in-flight". For
//     each, GetOrder learns its true status: Open / Filled / Canceled / Rejected,
//     or Missing if the broker never saw it (the send never landed).
//   - Position discrepancies are computed by comparing, per symbol, the signed
//     qty the journal's terminal fills imply against the broker's reported qty,
//     using exact decimal compare (no tolerance/float slop).
//   - Clean == (len(Discrepancies) == 0).
func ReconcileOnBoot(ctx context.Context, b Broker, journalPath string) (ReconcileResult, error) {
	replayed, err := orders.Replay(journalPath)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("reconcile: replay %s: %w", journalPath, err)
	}

	res := ReconcileResult{}

	// 1) Resolve in-flight orders against the broker.
	for _, rr := range replayed {
		if !rr.InFlight {
			continue
		}
		resolved, rerr := resolveInFlight(ctx, b, rr.Order)
		if rerr != nil {
			return ReconcileResult{}, rerr
		}
		res.Resolved = append(res.Resolved, resolved)
	}

	// 2) Compute journal-implied positions from TERMINAL fills only. An order's
	// fills count toward a position regardless of whether the order is fully
	// filled or partially filled then canceled — CumQty is real filled quantity.
	// We fold by symbol: BUY adds CumQty, SELL subtracts it (signed inventory).
	journalPos := make(map[string]orders.Decimal)
	for _, rr := range replayed {
		// In-flight orders are NOT settled journal positions: a crash can leave a
		// partial fill (nonzero CumQty) journaled with no terminal event, and
		// folding that here would manufacture a phantom discrepancy against the
		// broker's truth. In-flight fills are reconciled separately above via
		// resolveInFlight/GetOrder. Only terminal orders contribute to journalPos.
		if rr.InFlight {
			continue
		}
		o := rr.Order
		if o.CumQty.IsZero() {
			continue
		}
		signed := o.CumQty
		if o.Side == orders.SideSell {
			signed = signed.Neg()
		}
		cur := journalPos[o.Symbol] // zero value is numeric 0
		sum, aerr := cur.Add(signed)
		if aerr != nil {
			return ReconcileResult{}, fmt.Errorf("reconcile: sum journal qty for %s: %w", o.Symbol, aerr)
		}
		journalPos[o.Symbol] = sum
	}

	// 3) Pull broker positions and index by symbol.
	brokerPositions, perr := b.Positions(ctx)
	if perr != nil {
		return ReconcileResult{}, fmt.Errorf("reconcile: broker positions: %w", perr)
	}
	brokerPos := make(map[string]orders.Decimal, len(brokerPositions))
	for _, p := range brokerPositions {
		brokerPos[p.Symbol] = p.Qty
	}

	// 4) Diff every symbol seen on EITHER side. Exact decimal compare: equal
	// (including both-zero) is no discrepancy.
	seen := make(map[string]struct{}, len(journalPos)+len(brokerPos))
	var symbols []string
	for sym := range journalPos {
		if _, ok := seen[sym]; !ok {
			seen[sym] = struct{}{}
			symbols = append(symbols, sym)
		}
	}
	for sym := range brokerPos {
		if _, ok := seen[sym]; !ok {
			seen[sym] = struct{}{}
			symbols = append(symbols, sym)
		}
	}
	// Stable, symbol-sorted output for deterministic reports/tests.
	sortStrings(symbols)

	for _, sym := range symbols {
		jq := journalPos[sym] // zero value == 0 when absent
		bq := brokerPos[sym]
		if jq.Cmp(bq) != 0 {
			res.Discrepancies = append(res.Discrepancies, PositionDiscrepancy{
				Symbol:     sym,
				JournalQty: jq,
				BrokerQty:  bq,
			})
		}
	}

	res.Clean = len(res.Discrepancies) == 0
	return res, nil
}

// resolveInFlight asks the broker for the true state of one in-flight order and
// maps it to a ResolvedOrder. Not-found at the broker means the send never
// landed (Missing).
func resolveInFlight(ctx context.Context, b Broker, o *orders.Order) (ResolvedOrder, error) {
	out := ResolvedOrder{ClOrdID: o.ClOrdID, Symbol: o.Symbol}

	bo, err := b.GetOrder(ctx, o.ClOrdID)
	if err != nil {
		if isNotFound(err) {
			out.Status = ResolvedMissing
			return out, nil
		}
		return ResolvedOrder{}, fmt.Errorf("reconcile: get order %s: %w", o.ClOrdID, err)
	}

	out.Broker = bo
	switch bo.Status {
	case StatusFilled:
		out.Status = ResolvedFilled
	case StatusCanceled:
		out.Status = ResolvedCanceled
	case StatusRejected:
		out.Status = ResolvedRejected
	case StatusUnknown:
		// Broker returned an order but couldn't classify its status — treat as
		// missing rather than silently assuming it's live.
		out.Status = ResolvedMissing
	default: // StatusNew, StatusPartiallyFilled
		out.Status = ResolvedOpen
	}
	return out, nil
}
