package orders

import (
	"errors"
	"fmt"
)

// State is the explicit lifecycle position of an order. The machine is:
//
//	New ──► PartiallyFilled ──► Filled        (terminal)
//	  │            │
//	  ├────────────┴──► Canceled              (terminal)
//	  └───────────────► Rejected              (terminal)
//
// Filled, Canceled, and Rejected are terminal: no transition leaves them.
type State int

const (
	// StateNew is an order that has been created/sent but has no fills yet.
	StateNew State = iota
	// StatePartiallyFilled is an order with at least one fill but CumQty < OrderQty.
	StatePartiallyFilled
	// StateFilled is fully filled (CumQty == OrderQty). Terminal.
	StateFilled
	// StateCanceled was canceled before fully filling. Terminal.
	StateCanceled
	// StateRejected was rejected by the venue/risk layer. Terminal.
	StateRejected
)

// String renders the state for logs, the WAL, and tests.
func (s State) String() string {
	switch s {
	case StateNew:
		return "New"
	case StatePartiallyFilled:
		return "PartiallyFilled"
	case StateFilled:
		return "Filled"
	case StateCanceled:
		return "Canceled"
	case StateRejected:
		return "Rejected"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// IsTerminal reports whether no further transition is legal from s.
func (s State) IsTerminal() bool {
	switch s {
	case StateFilled, StateCanceled, StateRejected:
		return true
	default:
		return false
	}
}

// Side is the order direction.
type Side int

const (
	// SideBuy is a buy/long order.
	SideBuy Side = iota
	// SideSell is a sell/short order.
	SideSell
)

// String renders the side.
func (s Side) String() string {
	switch s {
	case SideBuy:
		return "Buy"
	case SideSell:
		return "Sell"
	default:
		return fmt.Sprintf("Side(%d)", int(s))
	}
}

// Sentinel errors. Callers use errors.Is to branch.
var (
	// ErrIllegalTransition is returned when a state transition is not allowed
	// by the machine (e.g. Filled -> PartiallyFilled, or any move out of a
	// terminal state).
	ErrIllegalTransition = errors.New("orders: illegal state transition")

	// ErrFillAlreadyApplied is returned (as a non-fatal signal) when ApplyFill
	// is called with a fillID that was already applied. The order is unchanged;
	// this lets replay/idempotent retries no-op safely.
	ErrFillAlreadyApplied = errors.New("orders: fill already applied")

	// ErrOverfill is returned when a fill would push CumQty above OrderQty.
	ErrOverfill = errors.New("orders: fill exceeds order quantity")

	// ErrFillOnTerminal is returned when a fill arrives on a terminal order that
	// is not eligible for fills (Canceled/Rejected, or an already-Filled order).
	ErrFillOnTerminal = errors.New("orders: fill on terminal order")

	// ErrInvalidFill is returned for a non-positive fill quantity or negative price.
	ErrInvalidFill = errors.New("orders: invalid fill")
)

// Order is a single working order with cumulative-fill accounting. All money
// fields use the Decimal type — never float64.
type Order struct {
	ClOrdID  string // deterministic client-order-ID (idempotency key)
	Strategy string
	Symbol   string
	Side     Side
	OrderQty Decimal // total quantity intended
	LimitPx  Decimal // limit price of the intent (0 for market)

	State     State
	CumQty    Decimal // total quantity filled so far
	AvgPx     Decimal // quantity-weighted average fill price
	LeavesQty Decimal // OrderQty - CumQty (remaining open)

	// appliedFills dedupes by exec/fill ID so a replayed or retried fill is
	// never double-counted.
	appliedFills map[string]struct{}
}

// NewOrder constructs a fresh order in StateNew with LeavesQty == OrderQty and
// zeroed fill accounting.
func NewOrder(clOrdID, strategy, symbol string, side Side, orderQty, limitPx Decimal) *Order {
	return &Order{
		ClOrdID:      clOrdID,
		Strategy:     strategy,
		Symbol:       symbol,
		Side:         side,
		OrderQty:     orderQty,
		LimitPx:      limitPx,
		State:        StateNew,
		CumQty:       ZeroDecimal,
		AvgPx:        ZeroDecimal,
		LeavesQty:    orderQty,
		appliedFills: make(map[string]struct{}),
	}
}

// legalTransitions encodes which target states are reachable from each state.
// Terminal states have no entries (nothing leaves them). Self-edges are allowed
// only where they are meaningful (a PartiallyFilled order taking another partial
// fill stays PartiallyFilled).
var legalTransitions = map[State]map[State]bool{
	StateNew: {
		StatePartiallyFilled: true,
		StateFilled:          true, // a single fill can complete the order
		StateCanceled:        true,
		StateRejected:        true,
	},
	StatePartiallyFilled: {
		StatePartiallyFilled: true, // additional partial fill
		StateFilled:          true,
		StateCanceled:        true, // cancel the remaining leaves
	},
	// Filled / Canceled / Rejected: terminal, no outgoing edges.
}

// canTransition reports whether moving from -> to is legal.
func canTransition(from, to State) bool {
	targets, ok := legalTransitions[from]
	if !ok {
		return false
	}
	return targets[to]
}

// transition moves the order to a new state, rejecting illegal moves. It does
// not touch fill accounting (callers that change quantities do that explicitly).
func (o *Order) transition(to State) error {
	if o.State == to && to == StatePartiallyFilled {
		return nil // staying partially filled across multiple partials is legal
	}
	if !canTransition(o.State, to) {
		return fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, o.State, to)
	}
	o.State = to
	return nil
}

// Cancel moves the order to Canceled. Legal only from New or PartiallyFilled;
// the remaining LeavesQty is closed out (set to zero).
func (o *Order) Cancel() error {
	if err := o.transition(StateCanceled); err != nil {
		return err
	}
	o.LeavesQty = ZeroDecimal
	return nil
}

// Reject moves the order to Rejected. Legal only from New.
func (o *Order) Reject() error {
	if err := o.transition(StateRejected); err != nil {
		return err
	}
	o.LeavesQty = ZeroDecimal
	return nil
}

// ApplyFill applies an execution to the order, deduped by fillID.
//
//   - Re-applying a known fillID is a no-op and returns ErrFillAlreadyApplied
//     (a signal, not a hard failure) so replays/retries can't double-count.
//   - CumQty += qty; LeavesQty = OrderQty - CumQty.
//   - AvgPx is the quantity-weighted average of all fill prices, computed in
//     exact decimal (no float drift): AvgPx = (oldAvg*oldCum + px*qty) / newCum.
//   - When CumQty == OrderQty the order becomes Filled; a non-final fill makes it
//     PartiallyFilled.
//   - An overfill (CumQty would exceed OrderQty) is rejected with ErrOverfill and
//     leaves the order unchanged.
func (o *Order) ApplyFill(fillID string, qty, px Decimal) error {
	if o.State.IsTerminal() {
		return fmt.Errorf("%w: state=%s", ErrFillOnTerminal, o.State)
	}
	if _, seen := o.appliedFills[fillID]; seen {
		return ErrFillAlreadyApplied
	}
	if qty.Sign() <= 0 || px.Sign() < 0 {
		return fmt.Errorf("%w: qty=%s px=%s", ErrInvalidFill, qty.String(), px.String())
	}

	newCum, err := o.CumQty.Add(qty)
	if err != nil {
		return fmt.Errorf("orders: cum add: %w", err)
	}
	if newCum.Cmp(o.OrderQty) > 0 {
		return fmt.Errorf("%w: cum=%s order=%s", ErrOverfill, newCum.String(), o.OrderQty.String())
	}

	// Weighted-average fill price, exact decimal (no float drift):
	//   AvgPx = (AvgPx*CumQty + px*qty) / newCum
	// computed as total notional / new cumulative quantity. newCum > 0 here
	// because qty > 0.
	prevNotional, err := o.AvgPx.Mul(o.CumQty)
	if err != nil {
		return fmt.Errorf("orders: prev notional: %w", err)
	}
	fillNotional, err := px.Mul(qty)
	if err != nil {
		return fmt.Errorf("orders: fill notional: %w", err)
	}
	totalNotional, err := prevNotional.Add(fillNotional)
	if err != nil {
		return fmt.Errorf("orders: total notional: %w", err)
	}
	newAvg, err := totalNotional.Quo(newCum)
	if err != nil {
		return fmt.Errorf("orders: avg px: %w", err)
	}

	newLeaves, err := o.OrderQty.Sub(newCum)
	if err != nil {
		return fmt.Errorf("orders: leaves: %w", err)
	}

	// Determine target state, then commit only if the transition is legal.
	target := StatePartiallyFilled
	if newCum.Cmp(o.OrderQty) == 0 {
		target = StateFilled
	}
	if err := o.transition(target); err != nil {
		return err
	}

	// Commit accounting (transition succeeded).
	o.CumQty = newCum
	o.AvgPx = newAvg
	o.LeavesQty = newLeaves
	o.appliedFills[fillID] = struct{}{}
	return nil
}
