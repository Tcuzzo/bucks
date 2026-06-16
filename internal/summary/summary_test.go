package summary

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"bucks/internal/analyst"
	"bucks/internal/channel"
	"bucks/internal/orders"
)

// --- test doubles -----------------------------------------------------------

// mockBackend is a deterministic analyst.Backend. It records the LAST prompt it was
// asked to complete (so a test can assert what was rendered into the prompt), and
// returns reply, or err when err != nil.
type mockBackend struct {
	name       string
	reply      string
	err        error
	calls      int
	lastPrompt string
}

func (m *mockBackend) Name() string { return m.name }

func (m *mockBackend) Complete(_ context.Context, prompt string) (string, error) {
	m.calls++
	m.lastPrompt = prompt
	if m.err != nil {
		return "", m.err
	}
	return m.reply, nil
}

// capturingLogger records every Printf so a test can assert the downgrade was logged.
type capturingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *capturingLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.TrimRight(format, "\n"))
	_ = args
}

func (l *capturingLogger) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func dec(t *testing.T, s string) orders.Decimal {
	t.Helper()
	d, err := orders.ParseDecimal(s)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", s, err)
	}
	return d
}

// sampleReport is a Report with realized P&L +125.50 and one AAPL position.
func sampleReport(t *testing.T) channel.Report {
	t.Helper()
	return channel.Report{
		Equity:       dec(t, "10125.50"),
		RealizedPL:   dec(t, "125.50"),
		UnrealizedPL: dec(t, "42.00"),
		Positions: []channel.Position{
			{
				Symbol:       "AAPL",
				Qty:          dec(t, "10"),
				MarkPx:       dec(t, "150.25"),
				UnrealizedPL: dec(t, "42.00"),
			},
		},
	}
}

// --- account summary: grounding --------------------------------------------

// TestSummarizeAccount_GroundsTrueNumber proves a model reply that states the REAL
// P&L (+125.50) grounds VERIFIED — it is not flagged as unverified.
func TestSummarizeAccount_GroundsTrueNumber(t *testing.T) {
	rep := sampleReport(t)
	be := &mockBackend{
		name:  "mock",
		reply: "Here's where you stand today: your realized P&L is +125.50 on the day. Paper trading isn't real money.",
	}
	s, err := SummarizeAccount(context.Background(), []analyst.Backend{be}, rep, "paper", false)
	if err != nil {
		t.Fatalf("SummarizeAccount: %v", err)
	}
	if len(s.Claims) == 0 {
		t.Fatal("expected at least one grounded account claim for the stated +125.50")
	}
	// The real number must be VERIFIED (not in the unverified set).
	if un := s.Unverified(); len(un) != 0 {
		t.Fatalf("true number +125.50 should ground verified; got unverified: %+v", un)
	}
	verifiedTrue := false
	for _, c := range s.Claims {
		if strings.Contains(c.Text, "125.50") && c.Verified {
			verifiedTrue = true
		}
	}
	if !verifiedTrue {
		t.Fatalf("expected the +125.50 claim VERIFIED; claims: %+v", s.Claims)
	}
}

// TestSummarizeAccount_FlagsFabricatedNumber proves a model reply that INVENTS a P&L
// (+999.99, no fact to back it) is FLAGGED unverified — never presented as fact.
func TestSummarizeAccount_FlagsFabricatedNumber(t *testing.T) {
	rep := sampleReport(t)
	be := &mockBackend{
		name:  "mock",
		reply: "Here's where you stand today: your P&L is up +999.99 — great day!",
	}
	s, err := SummarizeAccount(context.Background(), []analyst.Backend{be}, rep, "paper", false)
	if err != nil {
		t.Fatalf("SummarizeAccount: %v", err)
	}
	un := s.Unverified()
	if len(un) == 0 {
		t.Fatalf("fabricated +999.99 must be flagged unverified; claims: %+v", s.Claims)
	}
	foundFake := false
	for _, c := range un {
		if strings.Contains(c.Text, "999.99") {
			foundFake = true
		}
	}
	if !foundFake {
		t.Fatalf("the fabricated +999.99 should be in the unverified set; unverified: %+v", un)
	}
	// And it must NOT have been laundered as verified.
	for _, c := range s.Claims {
		if strings.Contains(c.Text, "999.99") && c.Verified {
			t.Fatalf("fabricated +999.99 was wrongly marked verified: %+v", c)
		}
	}
}

// TestSummarizeAccount_RendersRealFactsIntoPrompt proves the REAL facts (exact
// Decimal text — no float) are rendered into the prompt handed to the model: the
// true P&L and the position appear verbatim.
func TestSummarizeAccount_RendersRealFactsIntoPrompt(t *testing.T) {
	rep := sampleReport(t)
	be := &mockBackend{name: "mock", reply: "ok"}
	if _, err := SummarizeAccount(context.Background(), []analyst.Backend{be}, rep, "paper", false); err != nil {
		t.Fatalf("SummarizeAccount: %v", err)
	}
	p := be.lastPrompt
	for _, want := range []string{
		"realized_pnl = 125.50", // exact decimal P&L
		"equity = 10125.50",     // exact decimal equity
		"unrealized_pnl = 42.00",
		"AAPL",   // the position symbol
		"150.25", // the position mark (exact decimal)
		"mode = paper",
		"halted = no",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q; prompt:\n%s", want, p)
		}
	}
	// Guard: no float64 rendering artifacts (e.g. a value like 125.5 dropping a digit,
	// or scientific notation) — the exact Decimal string carries both decimals.
	if strings.Contains(p, "1.2550e") || strings.Contains(p, "realized_pnl = 125.5\n") {
		t.Errorf("prompt shows a non-exact money rendering; prompt:\n%s", p)
	}
}

// TestSummarizeAccount_Failover proves a downgrade is VISIBLE: when the primary
// errors the fallback answers, the Failovers trail records it, and the logger sees it.
func TestSummarizeAccount_Failover(t *testing.T) {
	rep := sampleReport(t)
	primary := &mockBackend{name: "primary", err: errors.New("503 unavailable")}
	fallback := &mockBackend{name: "fallback", reply: "Here's where you stand today: realized P&L 125.50, steady."}
	log := &capturingLogger{}

	s, err := SummarizeAccount(context.Background(), []analyst.Backend{primary, fallback}, rep, "live", false, WithLogger(log))
	if err != nil {
		t.Fatalf("SummarizeAccount: %v", err)
	}
	if s.Backend != "fallback" {
		t.Fatalf("expected surviving backend 'fallback', got %q", s.Backend)
	}
	if !s.Downgraded() || len(s.Failovers) != 1 {
		t.Fatalf("expected exactly one recorded failover; got %+v", s.Failovers)
	}
	if s.Failovers[0].From != "primary" || !strings.Contains(s.Failovers[0].Err, "503") {
		t.Fatalf("failover trail wrong: %+v", s.Failovers)
	}
	if !strings.Contains(log.joined(), "failing over") {
		t.Fatalf("downgrade should be logged (visible); log:\n%s", log.joined())
	}
}

// TestSummarizeAccount_AllBackendsFail proves a total failure fabricates NOTHING and
// returns a clear joined error naming both backends.
func TestSummarizeAccount_AllBackendsFail(t *testing.T) {
	rep := sampleReport(t)
	b1 := &mockBackend{name: "b1", err: errors.New("boom1")}
	b2 := &mockBackend{name: "b2", err: errors.New("boom2")}
	s, err := SummarizeAccount(context.Background(), []analyst.Backend{b1, b2}, rep, "paper", false)
	if err == nil {
		t.Fatal("expected an error when all backends fail")
	}
	if s.Text != "" {
		t.Fatalf("no summary text should be fabricated on total failure; got %q", s.Text)
	}
	for _, want := range []string{"b1", "b2", "boom1", "boom2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error missing %q; got: %v", want, err)
		}
	}
}

// TestSummarizeAccount_NoBackend proves an empty backend list is a clear error, not a
// panic.
func TestSummarizeAccount_NoBackend(t *testing.T) {
	_, err := SummarizeAccount(context.Background(), nil, sampleReport(t), "paper", false)
	if err == nil {
		t.Fatal("expected an error with no backend")
	}
	if !strings.Contains(err.Error(), "at least one backend") {
		t.Fatalf("expected a clear no-backend error; got: %v", err)
	}
}

// TestSummarizeAccount_HaltedAndModeRendered proves the halted flag and live mode are
// surfaced in the prompt (so the summary can be honest about both).
func TestSummarizeAccount_HaltedAndModeRendered(t *testing.T) {
	rep := sampleReport(t)
	be := &mockBackend{name: "mock", reply: "ok"}
	if _, err := SummarizeAccount(context.Background(), []analyst.Backend{be}, rep, "live", true); err != nil {
		t.Fatalf("SummarizeAccount: %v", err)
	}
	if !strings.Contains(be.lastPrompt, "mode = live") {
		t.Errorf("expected 'mode = live' in prompt; prompt:\n%s", be.lastPrompt)
	}
	if !strings.Contains(be.lastPrompt, "halted = yes") {
		t.Errorf("expected 'halted = yes' in prompt; prompt:\n%s", be.lastPrompt)
	}
}

// TestSummarizeAccount_ListMarkerNotFlagged proves a non-account number (a list
// marker like "1." with no account cue) is NOT flagged as an unverified figure —
// reusing the chat precision (cue-gated) so the summary doesn't look broken.
func TestSummarizeAccount_ListMarkerNotFlagged(t *testing.T) {
	rep := sampleReport(t)
	be := &mockBackend{
		name: "mock",
		// "3 things to watch" is a count, not an account figure; "2 percent" is a rule.
		reply: "Here's where you stand today: realized P&L 125.50. 3 things to watch this week. Always risk only 2 percent per idea.",
	}
	s, err := SummarizeAccount(context.Background(), []analyst.Backend{be}, rep, "paper", false)
	if err != nil {
		t.Fatalf("SummarizeAccount: %v", err)
	}
	// The true P&L grounds; the "3" and "2" are NOT account claims, so they must not
	// appear as unverified noise.
	for _, c := range s.Claims {
		if strings.Contains(c.Text, ": 3") || strings.HasSuffix(c.Text, " 2") {
			t.Fatalf("a list/rule number was wrongly treated as an account claim: %+v", c)
		}
	}
	if len(s.Unverified()) != 0 {
		t.Fatalf("no unverified figures expected (125.50 is real, 3/2 are not account claims); got %+v", s.Unverified())
	}
}

// --- ad-hoc summarize -------------------------------------------------------

// TestSummarize_CallsBackendWithTextAndInstruction proves Summarize hands the text
// AND the instruction to the backend and returns the reply.
func TestSummarize_CallsBackendWithTextAndInstruction(t *testing.T) {
	be := &mockBackend{name: "mock", reply: "Apple shipped a new chip; stock rose."}
	article := "Apple Inc. today announced its new M-series chip, and the stock climbed in after-hours trading."
	out, err := Summarize(context.Background(), []analyst.Backend{be}, article, "summarize for a beginner in one sentence")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if out.Text != "Apple shipped a new chip; stock rose." {
		t.Fatalf("unexpected summary text: %q", out.Text)
	}
	if !strings.Contains(be.lastPrompt, article) {
		t.Errorf("prompt should contain the source text; prompt:\n%s", be.lastPrompt)
	}
	if !strings.Contains(be.lastPrompt, "summarize for a beginner in one sentence") {
		t.Errorf("prompt should contain the instruction; prompt:\n%s", be.lastPrompt)
	}
	if out.Backend != "mock" {
		t.Errorf("expected backend 'mock', got %q", out.Backend)
	}
}

// TestSummarize_Failover proves the ad-hoc summarizer's downgrade is visible too.
func TestSummarize_Failover(t *testing.T) {
	primary := &mockBackend{name: "primary", err: errors.New("429 rate limit")}
	fallback := &mockBackend{name: "fallback", reply: "summary here"}
	out, err := Summarize(context.Background(), []analyst.Backend{primary, fallback}, "some text", "")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if out.Backend != "fallback" || !out.Downgraded() {
		t.Fatalf("expected a visible failover to 'fallback'; got %+v", out)
	}
	if len(out.Failovers) != 1 || out.Failovers[0].From != "primary" {
		t.Fatalf("failover trail wrong: %+v", out.Failovers)
	}
}

// TestSummarize_EmptyInputAndNoBackend proves empty text and no-backend are clear
// errors, not panics.
func TestSummarize_EmptyInputAndNoBackend(t *testing.T) {
	be := &mockBackend{name: "mock", reply: "x"}
	if _, err := Summarize(context.Background(), []analyst.Backend{be}, "   ", "do it"); err == nil {
		t.Fatal("expected an error on empty text")
	}
	if _, err := Summarize(context.Background(), nil, "text", "do it"); err == nil {
		t.Fatal("expected an error with no backend")
	}
}

// TestSummarize_AllBackendsFail proves a total ad-hoc failure returns a clear joined
// error and no fabricated summary.
func TestSummarize_AllBackendsFail(t *testing.T) {
	b1 := &mockBackend{name: "b1", err: errors.New("boom1")}
	b2 := &mockBackend{name: "b2", err: errors.New("boom2")}
	out, err := Summarize(context.Background(), []analyst.Backend{b1, b2}, "text", "")
	if err == nil {
		t.Fatal("expected an error when all backends fail")
	}
	if out.Text != "" {
		t.Fatalf("no text should be fabricated on total failure; got %q", out.Text)
	}
	for _, want := range []string{"b1", "b2", "boom1", "boom2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error missing %q; got: %v", want, err)
		}
	}
}
