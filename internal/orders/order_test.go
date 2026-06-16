package orders

import (
	"errors"
	"testing"
)

func dec(t *testing.T, s string) Decimal {
	t.Helper()
	d, err := ParseDecimal(s)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", s, err)
	}
	return d
}

func newTestOrder(t *testing.T, qty string) *Order {
	t.Helper()
	id := ClientOrderID("momentum", "AAPL", "entry", 1)
	return NewOrder(id, "momentum", "AAPL", SideBuy, dec(t, qty), dec(t, "150.00"))
}

func assertDecEq(t *testing.T, got Decimal, want string) {
	t.Helper()
	w := dec(t, want)
	if got.Cmp(w) != 0 {
		t.Fatalf("decimal mismatch: got %s, want %s", got.String(), want)
	}
}

// New order has the expected initial accounting.
func TestNewOrder_Initial(t *testing.T) {
	o := newTestOrder(t, "100")
	if o.State != StateNew {
		t.Fatalf("state=%s, want New", o.State)
	}
	assertDecEq(t, o.CumQty, "0")
	assertDecEq(t, o.LeavesQty, "100")
	assertDecEq(t, o.AvgPx, "0")
}

// Legal transitions: New->Canceled, New->Rejected.
func TestTransitions_Legal(t *testing.T) {
	o := newTestOrder(t, "100")
	if err := o.Cancel(); err != nil {
		t.Fatalf("New->Cancel: %v", err)
	}
	if o.State != StateCanceled {
		t.Fatalf("state=%s, want Canceled", o.State)
	}
	assertDecEq(t, o.LeavesQty, "0")

	o2 := newTestOrder(t, "100")
	if err := o2.Reject(); err != nil {
		t.Fatalf("New->Reject: %v", err)
	}
	if o2.State != StateRejected {
		t.Fatalf("state=%s, want Rejected", o2.State)
	}
}

// Illegal transitions are rejected with ErrIllegalTransition and state is unchanged.
func TestTransitions_Illegal(t *testing.T) {
	// Filled -> any move is illegal. Fill it fully first.
	o := newTestOrder(t, "100")
	if err := o.ApplyFill("f1", dec(t, "100"), dec(t, "150")); err != nil {
		t.Fatalf("full fill: %v", err)
	}
	if o.State != StateFilled {
		t.Fatalf("state=%s, want Filled", o.State)
	}
	// Filled -> Canceled illegal.
	if err := o.Cancel(); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("Filled->Cancel err=%v, want ErrIllegalTransition", err)
	}
	if o.State != StateFilled {
		t.Fatalf("state changed after illegal cancel: %s", o.State)
	}
	// Filled -> Reject illegal.
	if err := o.Reject(); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("Filled->Reject err=%v, want ErrIllegalTransition", err)
	}
	// Fill on a Filled (terminal) order is rejected as ErrFillOnTerminal.
	if err := o.ApplyFill("f2", dec(t, "1"), dec(t, "150")); !errors.Is(err, ErrFillOnTerminal) {
		t.Fatalf("fill on Filled err=%v, want ErrFillOnTerminal", err)
	}

	// Canceled -> Reject illegal; fill on Canceled illegal.
	c := newTestOrder(t, "100")
	if err := c.Cancel(); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := c.Reject(); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("Canceled->Reject err=%v, want ErrIllegalTransition", err)
	}
	if err := c.ApplyFill("fx", dec(t, "1"), dec(t, "150")); !errors.Is(err, ErrFillOnTerminal) {
		t.Fatalf("fill on Canceled err=%v, want ErrFillOnTerminal", err)
	}

	// Rejected -> Cancel illegal.
	r := newTestOrder(t, "100")
	if err := r.Reject(); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if err := r.Cancel(); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("Rejected->Cancel err=%v, want ErrIllegalTransition", err)
	}
}

// Partial then full fill -> Filled with correct CumQty/LeavesQty/AvgPx.
func TestApplyFill_PartialThenFull(t *testing.T) {
	o := newTestOrder(t, "100")

	if err := o.ApplyFill("f1", dec(t, "40"), dec(t, "150.00")); err != nil {
		t.Fatalf("partial fill 1: %v", err)
	}
	if o.State != StatePartiallyFilled {
		t.Fatalf("state=%s, want PartiallyFilled", o.State)
	}
	assertDecEq(t, o.CumQty, "40")
	assertDecEq(t, o.LeavesQty, "60")
	assertDecEq(t, o.AvgPx, "150.00")

	if err := o.ApplyFill("f2", dec(t, "60"), dec(t, "150.00")); err != nil {
		t.Fatalf("partial fill 2: %v", err)
	}
	if o.State != StateFilled {
		t.Fatalf("state=%s, want Filled", o.State)
	}
	assertDecEq(t, o.CumQty, "100")
	assertDecEq(t, o.LeavesQty, "0")
	assertDecEq(t, o.AvgPx, "150.00")
}

// Weighted-average price is exact across multiple partials at different prices —
// no float drift. 30@100 + 70@110 => avg 107 exactly.
func TestApplyFill_WeightedAvgExact(t *testing.T) {
	o := newTestOrder(t, "100")
	if err := o.ApplyFill("f1", dec(t, "30"), dec(t, "100")); err != nil {
		t.Fatalf("fill1: %v", err)
	}
	assertDecEq(t, o.AvgPx, "100")
	if err := o.ApplyFill("f2", dec(t, "70"), dec(t, "110")); err != nil {
		t.Fatalf("fill2: %v", err)
	}
	// (30*100 + 70*110)/100 = (3000+7700)/100 = 10700/100 = 107
	assertDecEq(t, o.AvgPx, "107")
	assertDecEq(t, o.CumQty, "100")
	if o.State != StateFilled {
		t.Fatalf("state=%s, want Filled", o.State)
	}
}

// A price that float64 cannot represent exactly must stay exact in decimal.
// 1@0.1 + 1@0.2 => avg 0.15 exactly (0.1+0.2 != 0.3 in float64).
func TestApplyFill_NoFloatDrift(t *testing.T) {
	id := ClientOrderID("mr", "BTC/USD", "entry", 1)
	o := NewOrder(id, "mr", "BTC/USD", SideBuy, dec(t, "2"), dec(t, "0"))
	if err := o.ApplyFill("f1", dec(t, "1"), dec(t, "0.1")); err != nil {
		t.Fatalf("fill1: %v", err)
	}
	if err := o.ApplyFill("f2", dec(t, "1"), dec(t, "0.2")); err != nil {
		t.Fatalf("fill2: %v", err)
	}
	assertDecEq(t, o.AvgPx, "0.15")
	assertDecEq(t, o.CumQty, "2")
}

// Fill dedup by fillID: applying the same fillID twice is a no-op and does not
// double-count.
func TestApplyFill_DedupByFillID(t *testing.T) {
	o := newTestOrder(t, "100")
	if err := o.ApplyFill("dup", dec(t, "40"), dec(t, "150")); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	err := o.ApplyFill("dup", dec(t, "40"), dec(t, "150"))
	if !errors.Is(err, ErrFillAlreadyApplied) {
		t.Fatalf("second apply err=%v, want ErrFillAlreadyApplied", err)
	}
	// No double count.
	assertDecEq(t, o.CumQty, "40")
	assertDecEq(t, o.LeavesQty, "60")
	if o.State != StatePartiallyFilled {
		t.Fatalf("state=%s after dup, want PartiallyFilled", o.State)
	}
}

// Overfill is rejected and the order is unchanged.
func TestApplyFill_OverfillRejected(t *testing.T) {
	o := newTestOrder(t, "100")
	if err := o.ApplyFill("f1", dec(t, "60"), dec(t, "150")); err != nil {
		t.Fatalf("fill1: %v", err)
	}
	// 60 + 50 = 110 > 100 => overfill.
	err := o.ApplyFill("f2", dec(t, "50"), dec(t, "150"))
	if !errors.Is(err, ErrOverfill) {
		t.Fatalf("overfill err=%v, want ErrOverfill", err)
	}
	// Unchanged.
	assertDecEq(t, o.CumQty, "60")
	assertDecEq(t, o.LeavesQty, "40")
	if o.State != StatePartiallyFilled {
		t.Fatalf("state=%s after overfill, want PartiallyFilled", o.State)
	}
	// The rejected fillID was not recorded, so a corrected, valid fill still works.
	if err := o.ApplyFill("f2", dec(t, "40"), dec(t, "150")); err != nil {
		t.Fatalf("corrected fill: %v", err)
	}
	assertDecEq(t, o.CumQty, "100")
	if o.State != StateFilled {
		t.Fatalf("state=%s, want Filled", o.State)
	}
}

// Invalid fills (zero/negative qty, negative px) are rejected.
func TestApplyFill_InvalidInputs(t *testing.T) {
	o := newTestOrder(t, "100")
	if err := o.ApplyFill("z", dec(t, "0"), dec(t, "150")); !errors.Is(err, ErrInvalidFill) {
		t.Fatalf("zero qty err=%v, want ErrInvalidFill", err)
	}
	if err := o.ApplyFill("n", dec(t, "-5"), dec(t, "150")); !errors.Is(err, ErrInvalidFill) {
		t.Fatalf("neg qty err=%v, want ErrInvalidFill", err)
	}
	if err := o.ApplyFill("np", dec(t, "5"), dec(t, "-150")); !errors.Is(err, ErrInvalidFill) {
		t.Fatalf("neg px err=%v, want ErrInvalidFill", err)
	}
	// State untouched.
	if o.State != StateNew {
		t.Fatalf("state=%s after invalid fills, want New", o.State)
	}
	assertDecEq(t, o.CumQty, "0")
}

func TestState_TerminalAndString(t *testing.T) {
	terminal := []State{StateFilled, StateCanceled, StateRejected}
	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Fatalf("%s should be terminal", s)
		}
	}
	nonTerminal := []State{StateNew, StatePartiallyFilled}
	for _, s := range nonTerminal {
		if s.IsTerminal() {
			t.Fatalf("%s should not be terminal", s)
		}
	}
	if StateNew.String() != "New" || StateFilled.String() != "Filled" {
		t.Fatalf("State.String wrong: %s %s", StateNew, StateFilled)
	}
	if SideBuy.String() != "Buy" || SideSell.String() != "Sell" {
		t.Fatalf("Side.String wrong: %s %s", SideBuy, SideSell)
	}
}
