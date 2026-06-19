package channel

import (
	"context"
	"errors"
	"testing"
	"time"
)

// atHour builds a deterministic local-time clock pinned to a given hour:minute on
// a fixed date, so the quiet-hours boundary tests are exact and timezone-stable
// (time.Local is used so IsQuiet reads the same wall hour the operator's config means).
func atHour(hour, min int) func() time.Time {
	t := time.Date(2026, 6, 19, hour, min, 0, 0, time.Local)
	return func() time.Time { return t }
}

func TestIsQuietBoundaries(t *testing.T) {
	q := NewQuietHours() // default 1..6
	cases := []struct {
		hour, min int
		want      bool
		why       string
	}{
		{1, 0, true, "01:00 is the start of quiet hours (inclusive)"},
		{0, 59, false, "00:59 is before quiet hours"},
		{5, 59, true, "05:59 is still inside quiet hours"},
		{6, 0, false, "06:00 is the end of quiet hours (exclusive) — operator wakes"},
		{12, 0, false, "noon is plainly not quiet"},
	}
	for _, c := range cases {
		got := q.IsQuiet(time.Date(2026, 6, 19, c.hour, c.min, 0, 0, time.Local))
		if got != c.want {
			t.Errorf("IsQuiet(%02d:%02d) = %v, want %v (%s)", c.hour, c.min, got, c.want, c.why)
		}
	}
}

func TestRoutineAlertHeldDuringQuietHours(t *testing.T) {
	inner := NewMockChannel()
	qc := NewQuietChannel(inner, WithNow(atHour(2, 30))) // 02:30 — quiet

	err := qc.SendAlert(context.Background(), Alert{Level: AlertInfo, Text: "heartbeat"})
	if err != nil {
		t.Fatalf("SendAlert returned error: %v", err)
	}
	if inner.AlertCount() != 0 {
		t.Errorf("routine alert during quiet hours reached inner channel (count=%d); want HELD", inner.AlertCount())
	}
	if qc.Held() != 1 {
		t.Errorf("Held() = %d, want 1 (the routine alert should be recorded as held)", qc.Held())
	}
}

func TestWarnAlertHeldDuringQuietHours(t *testing.T) {
	inner := NewMockChannel()
	qc := NewQuietChannel(inner, WithNow(atHour(3, 0))) // quiet

	// AlertWarn is notable but NOT an emergency — it is routine for quiet-hours purposes.
	if err := qc.SendAlert(context.Background(), Alert{Level: AlertWarn, Text: "drawdown nearing band"}); err != nil {
		t.Fatalf("SendAlert returned error: %v", err)
	}
	if inner.AlertCount() != 0 {
		t.Errorf("warn alert during quiet hours reached inner (count=%d); want HELD", inner.AlertCount())
	}
	if qc.Held() != 1 {
		t.Errorf("Held() = %d, want 1", qc.Held())
	}
}

func TestEmergencyAlertForwardedDuringQuietHours(t *testing.T) {
	inner := NewMockChannel()
	qc := NewQuietChannel(inner, WithNow(atHour(2, 30))) // quiet

	// AlertCritical is the emergency/breach level (kill switch) — must break through.
	if err := qc.SendAlert(context.Background(), Alert{Level: AlertCritical, Text: "KILL SWITCH"}); err != nil {
		t.Fatalf("SendAlert returned error: %v", err)
	}
	if inner.AlertCount() != 1 {
		t.Errorf("emergency alert during quiet hours was NOT forwarded (inner count=%d); want 1", inner.AlertCount())
	}
	if qc.Held() != 0 {
		t.Errorf("Held() = %d, want 0 (emergency must never be held)", qc.Held())
	}
}

func TestAlertForwardedOutsideQuietHours(t *testing.T) {
	inner := NewMockChannel()
	qc := NewQuietChannel(inner, WithNow(atHour(12, 0))) // noon — not quiet

	// Every level forwards outside quiet hours, including routine Info.
	for _, lvl := range []AlertLevel{AlertInfo, AlertWarn, AlertCritical} {
		if err := qc.SendAlert(context.Background(), Alert{Level: lvl, Text: "msg"}); err != nil {
			t.Fatalf("SendAlert(level=%v) returned error: %v", lvl, err)
		}
	}
	if inner.AlertCount() != 3 {
		t.Errorf("alerts outside quiet hours: inner count=%d; want 3 (all forwarded)", inner.AlertCount())
	}
	if qc.Held() != 0 {
		t.Errorf("Held() = %d, want 0 outside quiet hours", qc.Held())
	}
}

func TestReportHeldDuringQuietHoursAndForwardedOutside(t *testing.T) {
	// Held during quiet hours.
	innerQuiet := NewMockChannel()
	qcQuiet := NewQuietChannel(innerQuiet, WithNow(atHour(4, 0)))
	if err := qcQuiet.SendReport(context.Background(), Report{}); err != nil {
		t.Fatalf("SendReport (quiet) returned error: %v", err)
	}
	if innerQuiet.ReportCount() != 0 {
		t.Errorf("report during quiet hours reached inner (count=%d); want HELD", innerQuiet.ReportCount())
	}
	if qcQuiet.Held() != 1 {
		t.Errorf("Held() = %d, want 1 (report held)", qcQuiet.Held())
	}

	// Forwarded outside quiet hours.
	innerDay := NewMockChannel()
	qcDay := NewQuietChannel(innerDay, WithNow(atHour(9, 0)))
	if err := qcDay.SendReport(context.Background(), Report{}); err != nil {
		t.Fatalf("SendReport (day) returned error: %v", err)
	}
	if innerDay.ReportCount() != 1 {
		t.Errorf("report outside quiet hours: inner count=%d; want 1 (forwarded)", innerDay.ReportCount())
	}
	if qcDay.Held() != 0 {
		t.Errorf("Held() = %d, want 0 outside quiet hours", qcDay.Held())
	}
}

func TestRequestApprovalAlwaysForwarded(t *testing.T) {
	// During quiet hours, an approval request (operator-initiated, time-sensitive)
	// must still go through and return the inner channel's decision.
	inner := NewMockChannel().ScriptApprove()
	qc := NewQuietChannel(inner, WithNow(atHour(3, 0))) // quiet

	dec, err := qc.RequestApproval(context.Background(), ApprovalRequest{Summary: "BUY 10 AAPL"})
	if err != nil {
		t.Fatalf("RequestApproval returned error: %v", err)
	}
	if !dec.Approved() {
		t.Errorf("RequestApproval during quiet hours = %v; want Approved (must pass through to inner)", dec)
	}
	if inner.ApprovalCount() != 1 {
		t.Errorf("approval request did not reach inner (count=%d); want 1", inner.ApprovalCount())
	}
	if qc.Held() != 0 {
		t.Errorf("Held() = %d, want 0 (approvals are never held)", qc.Held())
	}
}

func TestRequestApprovalPropagatesDenyDuringQuietHours(t *testing.T) {
	// A denied/timed-out approval must propagate the fail-safe Denied unchanged.
	inner := NewMockChannel().ScriptTimeout()
	qc := NewQuietChannel(inner, WithNow(atHour(3, 0)))

	dec, err := qc.RequestApproval(context.Background(), ApprovalRequest{Summary: "BUY 10 AAPL"})
	if dec.Approved() {
		t.Errorf("timed-out approval = %v; want Denied (fail-safe must pass through)", dec)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("RequestApproval err = %v; want context.DeadlineExceeded propagated", err)
	}
	if inner.ApprovalCount() != 1 {
		t.Errorf("approval request did not reach inner (count=%d); want 1", inner.ApprovalCount())
	}
}

func TestInnerExposedForObservation(t *testing.T) {
	inner := NewMockChannel()
	qc := NewQuietChannel(inner)
	if qc.Inner() != inner {
		t.Errorf("Inner() did not return the wrapped channel")
	}
}

// compile-time assertion that QuietChannel satisfies the operator channel boundary.
var _ Channel = (*QuietChannel)(nil)
