package analyst

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"bucks/internal/orders"
	"bucks/internal/playbook"
)

// --- test doubles -----------------------------------------------------------

// mockBackend is a deterministic Backend for failover tests. It returns reply on
// Complete, or err when err != nil. calls counts invocations so a test can assert
// a backend was (or was NOT) reached.
type mockBackend struct {
	name  string
	reply string
	err   error
	calls int
}

func (m *mockBackend) Name() string { return m.name }

func (m *mockBackend) Complete(_ context.Context, _ string) (string, error) {
	m.calls++
	if m.err != nil {
		return "", m.err
	}
	return m.reply, nil
}

// capturingLogger records every Printf so a test can assert the downgrade was
// logged (visible), not silent.
type capturingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *capturingLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *capturingLogger) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func testPlaybook(t *testing.T) playbook.Playbook {
	t.Helper()
	pb, err := playbook.BuildPlaybook(map[string]string{
		playbook.KeyRiskTolerance: "moderate",
		playbook.KeyCapital:       "10000",
		playbook.KeyStyle:         "swing",
		playbook.KeyMaxDrawdown:   "0.20",
	})
	if err != nil {
		t.Fatalf("build test playbook: %v", err)
	}
	return pb
}

func testContext() MarketContext {
	return MarketContext{
		Symbol:  "AAPL",
		Summary: "uptrend, pulling back to the 20-day MA",
		Notes:   []string{"RSI=58", "ATR(14)=2.31"},
	}
}

const sampleReply = "LEAN: bullish\nRATIONALE: pullback to support in an uptrend\n"

// --- tests ------------------------------------------------------------------

// TestAnalyze_PrimaryProducesView proves the happy path: the primary backend
// answers, the View names it, and there are zero failovers.
func TestAnalyze_PrimaryProducesView(t *testing.T) {
	primary := &mockBackend{name: "primary", reply: sampleReply}
	secondary := &mockBackend{name: "secondary", reply: "LEAN: bearish\n"}
	a, err := New(testPlaybook(t), nil, primary, secondary)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v, err := a.Analyze(context.Background(), testContext(), nil)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if v.Backend != "primary" {
		t.Errorf("View.Backend = %q, want primary", v.Backend)
	}
	if v.Lean != LeanBullish {
		t.Errorf("View.Lean = %q, want bullish", v.Lean)
	}
	if v.Downgraded() {
		t.Errorf("View should not be downgraded; failovers = %+v", v.Failovers)
	}
	if secondary.calls != 0 {
		t.Errorf("secondary should not be called on a healthy primary; calls=%d", secondary.calls)
	}
}

// TestAnalyze_FailoverIsRecordedNotSilent is the core no-silent-downgrade test:
// the primary errors, the analyst fails over to the secondary, the secondary's
// View is returned, AND the failover is recorded in BOTH the View.Failovers trail
// and the logger. The downgrade is visible, never silent.
func TestAnalyze_FailoverIsRecordedNotSilent(t *testing.T) {
	primary := &mockBackend{name: "primary", err: errors.New("503 service unavailable")}
	secondary := &mockBackend{name: "secondary", reply: sampleReply}
	log := &capturingLogger{}
	a, err := New(testPlaybook(t), log, primary, secondary)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v, err := a.Analyze(context.Background(), testContext(), nil)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	// The surviving backend produced the View.
	if v.Backend != "secondary" {
		t.Errorf("View.Backend = %q, want secondary (failover)", v.Backend)
	}
	// The downgrade is recorded structurally.
	if !v.Downgraded() {
		t.Fatal("View.Downgraded() = false; expected the failover to be recorded")
	}
	if len(v.Failovers) != 1 {
		t.Fatalf("len(Failovers) = %d, want 1: %+v", len(v.Failovers), v.Failovers)
	}
	if v.Failovers[0].From != "primary" {
		t.Errorf("Failover.From = %q, want primary", v.Failovers[0].From)
	}
	if !strings.Contains(v.Failovers[0].Err, "503") {
		t.Errorf("Failover.Err = %q, want it to carry the underlying 503 error", v.Failovers[0].Err)
	}
	// And it is ALSO logged (human-visible), proving no silent downgrade.
	logged := log.joined()
	if !strings.Contains(logged, "primary") || !strings.Contains(strings.ToLower(logged), "failing over") {
		t.Errorf("failover not logged visibly; log was:\n%s", logged)
	}
	if primary.calls != 1 || secondary.calls != 1 {
		t.Errorf("calls: primary=%d secondary=%d, want 1/1", primary.calls, secondary.calls)
	}
}

// TestAnalyze_AllBackendsFail proves that when every backend errors, Analyze
// returns a clear error that wraps each underlying failure — and never fabricates
// a View.
func TestAnalyze_AllBackendsFail(t *testing.T) {
	primary := &mockBackend{name: "primary", err: errors.New("dns failure")}
	secondary := &mockBackend{name: "secondary", err: errors.New("401 unauthorized")}
	a, err := New(testPlaybook(t), nil, primary, secondary)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v, err := a.Analyze(context.Background(), testContext(), nil)
	if err == nil {
		t.Fatal("expected an error when all backends fail, got nil")
	}
	// No fabrication: the returned View is the FULL zero value — not just an empty
	// Backend. Nothing about a failed analysis may look like a real read.
	if v.Backend != "" {
		t.Errorf("expected empty View.Backend on total failure, got %q", v.Backend)
	}
	if v.Lean != "" {
		t.Errorf("expected empty View.Lean on total failure, got %q", v.Lean)
	}
	if v.Grounded {
		t.Error("expected View.Grounded=false on total failure (nothing was grounded)")
	}
	if v.Rationale != "" {
		t.Errorf("expected empty View.Rationale on total failure, got %q", v.Rationale)
	}
	if v.Symbol != "" {
		t.Errorf("expected empty View.Symbol on total failure, got %q", v.Symbol)
	}
	if len(v.Claims) != 0 || len(v.Failovers) != 0 {
		t.Errorf("expected no Claims/Failovers on total failure, got claims=%d failovers=%d",
			len(v.Claims), len(v.Failovers))
	}
	// The error wraps BOTH underlying failures.
	if !strings.Contains(err.Error(), "dns failure") || !strings.Contains(err.Error(), "401 unauthorized") {
		t.Errorf("error should wrap both backend failures, got: %v", err)
	}
}

// TestAnalyze_GroundsByConstruction is the BUG-2 contract test: Analyze runs the
// View through Ground before returning, so the returned View is grounded
// (Grounded=true) and each fact-bearing claim is labeled honestly — a claim backed
// by matching evidence comes back Verified, an unsupported one comes back
// Unverified. The View is grounded by construction; no caller has to remember to
// call Ground.
func TestAnalyze_GroundsByConstruction(t *testing.T) {
	// The model emits two fact-bearing claims: atr14 (matches the evidence) and
	// sharpe (no evidence supplied for it -> must stay unverified).
	reply := "LEAN: bullish\n" +
		"RATIONALE: pullback buy\n" +
		"CLAIM[atr14]: the 14-period ATR is 2.31\n" +
		"CLAIM[sharpe]: the strategy sharpe is 1.9\n"
	primary := &mockBackend{name: "primary", reply: reply}
	a, err := New(testPlaybook(t), nil, primary)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Only atr14 is backed by real evidence; sharpe is unsupported.
	v, err := a.Analyze(context.Background(), testContext(), map[string]string{"atr14": "2.31"})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if !v.Grounded {
		t.Fatal("Analyze must return a grounded View (Grounded=true) by construction")
	}
	atr := findClaim(t, v, "atr14")
	if !atr.Verified {
		t.Error("atr14 claim is backed by matching evidence -> must be Verified")
	}
	sharpe := findClaim(t, v, "sharpe")
	if sharpe.Verified {
		t.Error("sharpe claim has no supporting evidence -> must be Unverified, never silently fact")
	}
	// The unsupported claim is surfaced in the unverified set.
	foundSharpe := false
	for _, c := range v.UnverifiedClaims() {
		if c.EvidenceKey == "sharpe" {
			foundSharpe = true
		}
	}
	if !foundSharpe {
		t.Error("unsupported sharpe claim must appear in UnverifiedClaims()")
	}
}

// TestAnalyze_NilEvidenceMarksAllUnverified proves the honest default: when a
// caller passes nil/empty evidence, the returned View is still grounded but EVERY
// fact-bearing claim is Unverified — Analyze never silently verifies a claim it
// has no evidence for.
func TestAnalyze_NilEvidenceMarksAllUnverified(t *testing.T) {
	reply := "LEAN: bullish\nCLAIM[atr14]: the 14-period ATR is 2.31\nCLAIM[rsi]: RSI is 58\n"
	a, err := New(testPlaybook(t), nil, &mockBackend{name: "p", reply: reply})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v, err := a.Analyze(context.Background(), testContext(), nil)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if !v.Grounded {
		t.Fatal("View must still be grounded even with nil evidence")
	}
	for _, c := range v.Claims {
		if c.Verified {
			t.Errorf("claim %q must be Unverified with nil evidence, got Verified", c.Text)
		}
	}
}

// TestNew_RequiresABackend proves an empty backend list is rejected (a
// programming error surfaced, not silently tolerated).
func TestNew_RequiresABackend(t *testing.T) {
	if _, err := New(testPlaybook(t), nil); err == nil {
		t.Fatal("New with no backends should error, got nil")
	}
}

// TestBuildPrompt_IncludesPlaybookAndContext proves the prompt is grounded in the
// owner's playbook and the live market context (the analysis is playbook-driven).
func TestBuildPrompt_IncludesPlaybookAndContext(t *testing.T) {
	pb := testPlaybook(t)
	a, err := New(pb, nil, &mockBackend{name: "p", reply: sampleReply})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	prompt := a.buildPrompt(testContext())
	for _, want := range []string{"moderate", "swing", "AAPL", "uptrend", "RSI=58"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q; prompt was:\n%s", want, prompt)
		}
	}
}

// ensure orders is used (keeps the import honest if a future edit drops the only
// reference); a zero Decimal is a valid sanity anchor.
var _ = orders.ZeroDecimal
