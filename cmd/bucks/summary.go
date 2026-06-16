package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"bucks/internal/analyst"
	"bucks/internal/channel"
	"bucks/internal/orders"
	"bucks/internal/summary"
)

// runSummaryStdio is the production `bucks summary` entry point: it resolves the
// backend from the SAME chat env vars (BUCKS_CHAT_BASEURL/_KEY/_MODEL) and writes a
// plain-English account summary to stdout. With no backend it prints a clear message
// and exits 0 — it never crashes for lack of an LLM.
func runSummaryStdio() error {
	return runSummary(os.Stdout, envSummaryBackends, demoSnapshot())
}

// snapshot is the account state a summary is built from: the Report plus the mode
// (paper/live) and halted flag. In this slice the live state wiring lands with the
// loop; the CLI uses a demo snapshot when no live state is present, so the command
// always produces an honest, grounded example without inventing live numbers.
type snapshot struct {
	report channel.Report
	mode   string
	halted bool
}

// backendsFactory builds the ordered backends to summarize over. Returning (nil,
// nil) means "no backend configured" — the CLI prints a clear message and exits 0.
// It is an injection seam so the default suite drives the command with a mock
// backend (no network), exactly like the chat command's chatterFactory.
type backendsFactory func() ([]analyst.Backend, error)

// envSummaryBackends builds the backend from the chat env vars, or returns (nil,
// nil) when no endpoint is configured. It shares the SINGLE backend-selection seam
// (envChatBackend) with the chat REPL, so a BUCKS_CHAT_PROVIDER choice (the free
// NVIDIA Nemotron path, Groq, etc.) applies identically to summaries — the two
// commands can never drift in how they pick a model.
func envSummaryBackends() ([]analyst.Backend, error) {
	backend, err := envChatBackend()
	if err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, nil
	}
	return []analyst.Backend{backend}, nil
}

// runSummary builds and prints a plain-English account summary from the snapshot
// using the injected backends. With NO backend (factory returns nil) it prints a
// clear message and exits 0. in/out + factory are injected so the default suite
// drives the real entry point with a mock backend (offline).
func runSummary(out io.Writer, newBackends backendsFactory, snap snapshot) error {
	backends, err := newBackends()
	if err != nil {
		return fmt.Errorf("summary: %w", err)
	}
	if len(backends) == 0 {
		fmt.Fprint(out, noBackendMessage("summary"))
		return nil
	}

	s, err := summary.SummarizeAccount(context.Background(), backends, snap.report, snap.mode, snap.halted)
	if err != nil {
		return fmt.Errorf("summary: %w", err)
	}
	fmt.Fprintln(out, s.Text)
	// Surface routing + honesty provenance: a downgrade and any unbacked figure are
	// never hidden from the owner.
	if s.Downgraded() {
		fmt.Fprintf(out, "(note: answered on fallback model %q after %d failover(s))\n",
			s.Backend, len(s.Failovers))
	}
	if un := s.Unverified(); len(un) > 0 {
		fmt.Fprintf(out, "(heads up: %d figure(s) above are NOT backed by your real account facts — treat as unverified)\n", len(un))
	}
	return nil
}

// demoSnapshot is a small, honest example used when no live state is wired: a paper
// account with one position, exact-decimal figures. It lets `bucks summary` produce
// a real, grounded summary out of the box without inventing live numbers.
func demoSnapshot() snapshot {
	return snapshot{
		report: channel.Report{
			Equity:       orders.MustParseDecimal("10125.50"),
			RealizedPL:   orders.MustParseDecimal("125.50"),
			UnrealizedPL: orders.MustParseDecimal("42.00"),
			Positions: []channel.Position{
				{
					Symbol:       "AAPL",
					Qty:          orders.MustParseDecimal("10"),
					MarkPx:       orders.MustParseDecimal("150.25"),
					UnrealizedPL: orders.MustParseDecimal("42.00"),
				},
			},
		},
		mode:   "paper",
		halted: false,
	}
}
