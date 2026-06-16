package channel

import (
	"context"
	"errors"
	"testing"
	"time"

	"bucks/internal/orders"
)

func dec(t *testing.T, s string) Decimal {
	t.Helper()
	d, err := orders.ParseDecimal(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return d
}

// TestMockChannel_DefaultDeniesFailSafe proves an unscripted approval request
// fails SAFE: with no script the mock returns DecisionDenied (silence is a no).
func TestMockChannel_DefaultDeniesFailSafe(t *testing.T) {
	m := NewMockChannel()
	d, err := m.RequestApproval(context.Background(), ApprovalRequest{Symbol: "AAPL"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Approved() {
		t.Fatalf("unscripted approval must fail-safe to Denied, got %s", d)
	}
	if m.ApprovalCount() != 1 {
		t.Fatalf("want 1 request recorded, got %d", m.ApprovalCount())
	}
}

// TestMockChannel_ScriptApprove proves a scripted approve returns Approved exactly
// once, then the queue empties back to the fail-safe default.
func TestMockChannel_ScriptApprove(t *testing.T) {
	m := NewMockChannel()
	m.ScriptApprove()
	d, err := m.RequestApproval(context.Background(), ApprovalRequest{Symbol: "AAPL"})
	if err != nil || !d.Approved() {
		t.Fatalf("scripted approve: got (%s,%v), want (Approved,nil)", d, err)
	}
	// Queue now empty -> fail-safe Denied.
	d2, _ := m.RequestApproval(context.Background(), ApprovalRequest{Symbol: "AAPL"})
	if d2.Approved() {
		t.Fatalf("after script exhausted, must be Denied, got %s", d2)
	}
}

// TestMockChannel_ScriptDeny proves a scripted deny returns Denied with no error.
func TestMockChannel_ScriptDeny(t *testing.T) {
	m := NewMockChannel()
	m.ScriptDeny()
	d, err := m.RequestApproval(context.Background(), ApprovalRequest{Symbol: "AAPL"})
	if err != nil {
		t.Fatalf("scripted deny error: %v", err)
	}
	if d.Approved() {
		t.Fatalf("scripted deny must be Denied, got %s", d)
	}
}

// TestMockChannel_ScriptTimeout proves a scripted timeout returns the fail-safe
// Denied AND the deadline error — exactly how a real timed-out transport behaves.
func TestMockChannel_ScriptTimeout(t *testing.T) {
	m := NewMockChannel()
	m.ScriptTimeout()
	d, err := m.RequestApproval(context.Background(), ApprovalRequest{Symbol: "AAPL"})
	if d.Approved() {
		t.Fatalf("timeout must be Denied, got %s", d)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout must carry DeadlineExceeded, got %v", err)
	}
}

// TestMockChannel_ExpiredContextDenies proves an already-expired context is treated
// as a timeout -> fail-safe Denied with the ctx error.
func TestMockChannel_ExpiredContextDenies(t *testing.T) {
	m := NewMockChannel()
	m.ScriptApprove() // even with an approve scripted, an expired ctx denies first
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	d, err := m.RequestApproval(ctx, ApprovalRequest{Symbol: "AAPL"})
	if d.Approved() {
		t.Fatalf("expired context must deny, got %s", d)
	}
	if err == nil {
		t.Fatalf("expired context must return the ctx error")
	}
}

// TestMockChannel_FailApprovalIsSafe proves a forced transport error fails SAFE to
// Denied.
func TestMockChannel_FailApprovalIsSafe(t *testing.T) {
	m := NewMockChannel()
	boom := errors.New("transport down")
	m.FailApproval(boom)
	d, err := m.RequestApproval(context.Background(), ApprovalRequest{Symbol: "AAPL"})
	if d.Approved() {
		t.Fatalf("transport failure must fail-safe to Denied, got %s", d)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("want the transport error, got %v", err)
	}
}

// TestMockChannel_RecordsAlertsAndReports proves alerts/reports are recorded in
// order and carry their payloads.
func TestMockChannel_RecordsAlertsAndReports(t *testing.T) {
	m := NewMockChannel()
	clk := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	if err := m.SendAlert(context.Background(), Alert{Level: AlertInfo, Text: "hb", Time: clk}); err != nil {
		t.Fatalf("send alert: %v", err)
	}
	rep := Report{GeneratedAt: clk, Equity: dec(t, "10000"), RealizedPL: dec(t, "12.50")}
	if err := m.SendReport(context.Background(), rep); err != nil {
		t.Fatalf("send report: %v", err)
	}
	if m.AlertCount() != 1 || m.ReportCount() != 1 {
		t.Fatalf("want 1 alert + 1 report, got %d + %d", m.AlertCount(), m.ReportCount())
	}
	got := m.Alerts()[0]
	if got.Text != "hb" || !got.Time.Equal(clk) {
		t.Fatalf("alert payload mismatch: %+v", got)
	}
	if rep := m.Reports()[0]; rep.RealizedPL.Cmp(dec(t, "12.50")) != 0 {
		t.Fatalf("report PnL mismatch: %s", rep.RealizedPL.String())
	}
}

// TestMockChannel_SendFailuresSurfaceButRecord proves a forced send error is
// returned to the caller while the attempt is still recorded (no silent drop).
func TestMockChannel_SendFailuresSurfaceButRecord(t *testing.T) {
	m := NewMockChannel()
	boom := errors.New("nope")
	m.FailAlert(boom)
	m.FailReport(boom)
	if err := m.SendAlert(context.Background(), Alert{Text: "x"}); !errors.Is(err, boom) {
		t.Fatalf("alert error not surfaced: %v", err)
	}
	if err := m.SendReport(context.Background(), Report{}); !errors.Is(err, boom) {
		t.Fatalf("report error not surfaced: %v", err)
	}
	if m.AlertCount() != 1 || m.ReportCount() != 1 {
		t.Fatalf("failed sends must still be recorded; got %d + %d", m.AlertCount(), m.ReportCount())
	}
}

// TestDecision_ZeroValueIsDenied proves the Decision zero value is the fail-safe
// Denied (so any unset/defaulted decision never authorizes a trade).
func TestDecision_ZeroValueIsDenied(t *testing.T) {
	var d Decision // zero value
	if d.Approved() {
		t.Fatalf("zero-value Decision must be Denied (fail-safe), got %s", d)
	}
	if d.String() != "Denied" {
		t.Fatalf("zero-value Decision String should be Denied, got %s", d.String())
	}
}
