package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"bucks/internal/analyst"
)

// summaryScriptBackend is a deterministic mock analyst.Backend for the summary CLI
// test: it returns a fixed reply so the command is exercised without any network.
type summaryScriptBackend struct {
	name  string
	reply string
}

func (b *summaryScriptBackend) Name() string { return b.name }

func (b *summaryScriptBackend) Complete(_ context.Context, _ string) (string, error) {
	return b.reply, nil
}

// mockBackendsFactory builds an ordered backend list over the script backend — the
// injection seam that keeps the summary CLI test offline.
func mockBackendsFactory(reply string) backendsFactory {
	return func() ([]analyst.Backend, error) {
		return []analyst.Backend{&summaryScriptBackend{name: "mock", reply: reply}}, nil
	}
}

// TestRunSummary_DispatchesAndPrints proves `bucks summary` builds a summary from the
// demo snapshot and PRINTS it — the real entry point, driven with a mock backend
// (no network). The true demo P&L (125.50) grounds, so no unverified heads-up fires.
func TestRunSummary_DispatchesAndPrints(t *testing.T) {
	var out bytes.Buffer
	reply := "Here's where you stand today: your realized P&L is 125.50 on paper. Nothing is promised."
	if err := runSummary(&out, mockBackendsFactory(reply), demoSnapshot()); err != nil {
		t.Fatalf("runSummary: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Here's where you stand today") {
		t.Errorf("summary text not printed; output:\n%s", got)
	}
	if !strings.Contains(got, "125.50") {
		t.Errorf("summary should print the grounded P&L; output:\n%s", got)
	}
	// The real number grounds, so the unverified heads-up must NOT appear.
	if strings.Contains(got, "unverified") {
		t.Errorf("true figure grounded; no unverified note expected; output:\n%s", got)
	}
}

// TestRunSummary_FlagsFabricatedFigure proves the CLI surfaces an unbacked figure: a
// model that invents +999.99 triggers the "unverified" heads-up on the real entry point.
func TestRunSummary_FlagsFabricatedFigure(t *testing.T) {
	var out bytes.Buffer
	reply := "Here's where you stand today: your P&L is up +999.99 — amazing!"
	if err := runSummary(&out, mockBackendsFactory(reply), demoSnapshot()); err != nil {
		t.Fatalf("runSummary: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "unverified") {
		t.Errorf("fabricated figure should trigger the unverified note; output:\n%s", got)
	}
}

// TestRunSummary_NoBackendIsClearNotCrash proves the no-config path: with no backend
// (factory returns nil) the command prints a clear message naming the env vars and
// returns nil — it never crashes for lack of an LLM.
func TestRunSummary_NoBackendIsClearNotCrash(t *testing.T) {
	var out bytes.Buffer
	noBackend := func() ([]analyst.Backend, error) { return nil, nil }
	if err := runSummary(&out, noBackend, demoSnapshot()); err != nil {
		t.Fatalf("runSummary with no backend should not error, got: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "no LLM backend configured") {
		t.Errorf("no-backend message missing; output:\n%s", got)
	}
	if !strings.Contains(got, envChatBaseURL) {
		t.Errorf("no-backend message should name %s; output:\n%s", envChatBaseURL, got)
	}
}

// TestRunSummary_DispatchedFromRun proves the actual CLI dispatch reaches the summary
// path. With no chat env set, it prints the clear no-backend message and exits 0 —
// proving the entry point is wired without needing a live model.
func TestRunSummary_DispatchedFromRun(t *testing.T) {
	t.Setenv(envChatBaseURL, "")  // ensure no Ollama endpoint is configured in the test env
	t.Setenv(envChatProvider, "") // and no OpenAI-compatible provider, so the path stays offline
	if err := run([]string{"summary"}); err != nil {
		t.Fatalf("`bucks summary` dispatch errored: %v", err)
	}
}
