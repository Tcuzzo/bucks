package channel

import (
	"context"
	"sync"
)

// MockChannel is an in-memory, deterministic Channel for tests and for driving the
// rest of the BUCKS build without a live operator surface. It makes NO network
// calls — that is the whole point: the default test suite can never page a real
// operator. It records every alert and report sent, and answers approval requests
// from a scripted queue (approve / deny / timeout), so a test asserts exactly what
// the trader said and did.
//
// Concurrency: all state is mutex-guarded, so the loop's worker goroutine and the
// test goroutine can touch it safely.
type MockChannel struct {
	mu sync.Mutex

	alerts   []Alert
	reports  []Report
	requests []ApprovalRequest

	// scripted approval answers, consumed in order. Each entry is one response to
	// the next RequestApproval call. When the queue is exhausted the default
	// behavior (defaultApproval) applies.
	answers []scriptedAnswer
	// defaultApproval is returned (with a nil error) once the scripted queue is
	// empty. Defaults to DecisionDenied — fail-safe even when unscripted.
	defaultApproval Decision

	// failAlert / failReport / failApproval, when non-nil, force the matching
	// method to return that error (still recording the attempt) so error paths are
	// testable without a real transport.
	failAlert    error
	failReport   error
	failApproval error
}

// scriptedAnswer is one queued response to RequestApproval: the decision to return
// and the error to return alongside it. A timeout is modeled as
// {DecisionDenied, context.DeadlineExceeded} — the mock returns the fail-safe
// Denied with the deadline error, exactly as a real timed-out transport would.
type scriptedAnswer struct {
	decision Decision
	err      error
}

// NewMockChannel constructs an empty MockChannel that, with no script, denies
// every approval (fail-safe) and records all alerts/reports.
func NewMockChannel() *MockChannel {
	return &MockChannel{defaultApproval: DecisionDenied}
}

// ScriptApprove queues an explicit Approved answer for the next RequestApproval.
func (m *MockChannel) ScriptApprove() *MockChannel {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answers = append(m.answers, scriptedAnswer{decision: DecisionApproved})
	return m
}

// ScriptDeny queues an explicit Denied answer for the next RequestApproval.
func (m *MockChannel) ScriptDeny() *MockChannel {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answers = append(m.answers, scriptedAnswer{decision: DecisionDenied})
	return m
}

// ScriptTimeout queues a timeout for the next RequestApproval: the mock returns
// the fail-safe DecisionDenied together with context.DeadlineExceeded, exactly as
// a real timed-out transport would, so the trader's "timeout => no trade" path is
// exercised precisely.
func (m *MockChannel) ScriptTimeout() *MockChannel {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answers = append(m.answers, scriptedAnswer{decision: DecisionDenied, err: context.DeadlineExceeded})
	return m
}

// FailAlert forces SendAlert to return err (the attempt is still recorded).
func (m *MockChannel) FailAlert(err error) { m.mu.Lock(); m.failAlert = err; m.mu.Unlock() }

// FailReport forces SendReport to return err (the attempt is still recorded).
func (m *MockChannel) FailReport(err error) { m.mu.Lock(); m.failReport = err; m.mu.Unlock() }

// FailApproval forces RequestApproval to return (DecisionDenied, err) — a
// transport failure, which fails SAFE to Denied (the attempt is still recorded).
func (m *MockChannel) FailApproval(err error) { m.mu.Lock(); m.failApproval = err; m.mu.Unlock() }

// SendAlert records the alert and returns the configured failAlert (nil by
// default). No network.
func (m *MockChannel) SendAlert(ctx context.Context, a Alert) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alerts = append(m.alerts, a)
	return m.failAlert
}

// RequestApproval records the request and returns the next scripted answer (or the
// fail-safe default when the script is exhausted). A configured failApproval short-
// circuits to (DecisionDenied, err) — fail-safe. It honors a context that is
// already canceled/expired by returning the fail-safe Denied with the ctx error.
func (m *MockChannel) RequestApproval(ctx context.Context, r ApprovalRequest) (Decision, error) {
	if err := ctx.Err(); err != nil {
		// An already-expired context is itself a timeout: deny, fail-safe.
		m.mu.Lock()
		m.requests = append(m.requests, r)
		m.mu.Unlock()
		return DecisionDenied, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, r)
	if m.failApproval != nil {
		return DecisionDenied, m.failApproval
	}
	if len(m.answers) > 0 {
		ans := m.answers[0]
		m.answers = m.answers[1:]
		// A scripted non-approval always normalizes to the fail-safe Denied.
		if ans.decision != DecisionApproved {
			return DecisionDenied, ans.err
		}
		return DecisionApproved, ans.err
	}
	return m.defaultApproval, nil
}

// SendReport records the report and returns the configured failReport (nil by
// default). No network.
func (m *MockChannel) SendReport(ctx context.Context, r Report) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reports = append(m.reports, r)
	return m.failReport
}

// Alerts returns a copy of every alert sent so far, in send order.
func (m *MockChannel) Alerts() []Alert {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Alert, len(m.alerts))
	copy(out, m.alerts)
	return out
}

// Reports returns a copy of every report sent so far, in send order.
func (m *MockChannel) Reports() []Report {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Report, len(m.reports))
	copy(out, m.reports)
	return out
}

// ApprovalRequests returns a copy of every approval request received, in order.
func (m *MockChannel) ApprovalRequests() []ApprovalRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ApprovalRequest, len(m.requests))
	copy(out, m.requests)
	return out
}

// AlertCount / ReportCount / ApprovalCount are convenience counters for tests.
func (m *MockChannel) AlertCount() int    { m.mu.Lock(); defer m.mu.Unlock(); return len(m.alerts) }
func (m *MockChannel) ReportCount() int   { m.mu.Lock(); defer m.mu.Unlock(); return len(m.reports) }
func (m *MockChannel) ApprovalCount() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.requests) }

// compile-time assertion that MockChannel satisfies the interface.
var _ Channel = (*MockChannel)(nil)
