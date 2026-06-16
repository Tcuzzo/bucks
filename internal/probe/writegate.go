package probe

import (
	"errors"
	"fmt"
	"sync"
)

// Errors returned by the write gate. Reads are ALWAYS allowed (you can't lose
// money by reading), so there is no read gate — only these write preconditions.
var (
	// ErrReadProbeRequired means the live read-only probe has not run yet, so the
	// surface is unproven and writes stay locked.
	ErrReadProbeRequired = errors.New("probe: writes locked — live read-only probe has not proven the surface")
	// ErrPaperRunRequired means no paper/dry-run has succeeded yet.
	ErrPaperRunRequired = errors.New("probe: writes locked — a paper/dry-run order has not succeeded")
	// ErrOperatorConfirmRequired means the operator has not confirmed writes in
	// plain English (the operator-authority/approval-gate law).
	ErrOperatorConfirmRequired = errors.New("probe: writes locked — operator has not confirmed writes")
)

// WriteGate is the safety latch that keeps order placement (any WRITE) LOCKED
// until three independent conditions are all met:
//
//  1. the live read-only probe has proven the surface (markReadProbed),
//  2. a paper/dry-run order has succeeded (RecordPaperRun), and
//  3. the operator has confirmed in plain English (ConfirmWrites(true)).
//
// Reads are never gated. AssertWriteAllowed returns the FIRST unmet precondition
// as an error, so a caller learns exactly what is still missing. The gate is
// safe for concurrent use. This binds the operator-authority law: writes can move
// money, so they require an explicit human go-ahead — risky reads do not.
type WriteGate struct {
	mu         sync.Mutex
	readProbed bool
	paperRun   bool
	operatorOK bool
}

// NewWriteGate returns a fresh, fully-LOCKED gate: every write precondition is
// unmet. Reads are allowed immediately (there is no read gate).
func NewWriteGate() *WriteGate { return &WriteGate{} }

// markReadProbed records that the live read-only probe has run and proven the
// surface. It is unexported because only the probe pipeline may assert this — a
// caller cannot fake "the surface was probed" to unlock writes.
func (g *WriteGate) markReadProbed() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.readProbed = true
}

// ReadProbed reports whether the live read-only probe has run (read-only view).
func (g *WriteGate) ReadProbed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.readProbed
}

// RecordPaperRun records that a paper/dry-run order succeeded. It refuses to do so
// until the read-probe has proven the surface — you cannot "paper-trade" a surface
// the probe never confirmed (fail SAFE, in order).
func (g *WriteGate) RecordPaperRun() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.readProbed {
		return ErrReadProbeRequired
	}
	g.paperRun = true
	return nil
}

// PaperRunPassed reports whether a paper/dry-run has succeeded.
func (g *WriteGate) PaperRunPassed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paperRun
}

// ConfirmWrites records the operator's explicit plain-English decision. Passing
// false (or never calling it) leaves writes locked. Passing true satisfies the
// third precondition — but only matters once the read-probe and paper-run are
// also done; AssertWriteAllowed enforces the full conjunction.
func (g *WriteGate) ConfirmWrites(operatorApproved bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.operatorOK = operatorApproved
}

// OperatorConfirmed reports whether the operator has approved writes.
func (g *WriteGate) OperatorConfirmed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.operatorOK
}

// AssertWriteAllowed returns nil only when ALL three preconditions are met; it
// returns the first unmet one as an error otherwise. Call this immediately before
// any state-changing broker call (PlaceOrder/CancelOrder). It is the single
// chokepoint the rest of BUCKS routes writes through.
func (g *WriteGate) AssertWriteAllowed() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case !g.readProbed:
		return ErrReadProbeRequired
	case !g.paperRun:
		return ErrPaperRunRequired
	case !g.operatorOK:
		return ErrOperatorConfirmRequired
	default:
		return nil
	}
}

// AssertReadAllowed always returns nil: reads are never gated. It exists so call
// sites can route every broker call through an explicit allow check symmetrically
// (read vs write) and make the "reads are free" guarantee obvious in the code.
func (g *WriteGate) AssertReadAllowed() error { return nil }

// Status renders a short human description of which preconditions are met, for
// operator-facing plain-English reports.
func (g *WriteGate) Status() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return fmt.Sprintf("read-probed=%t paper-run=%t operator-confirmed=%t writes-allowed=%t",
		g.readProbed, g.paperRun, g.operatorOK,
		g.readProbed && g.paperRun && g.operatorOK)
}
