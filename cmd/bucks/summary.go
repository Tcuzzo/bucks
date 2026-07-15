package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"bucks/internal/analyst"
	"bucks/internal/channel"
	"bucks/internal/orders"
	"bucks/internal/risk"
	"bucks/internal/secrets"
	"bucks/internal/summary"
)

// runSummaryStdio is the production `bucks summary` entry point: it resolves the
// backend from the SAME chat env vars (BUCKS_CHAT_BASEURL/_KEY/_MODEL) and writes a
// plain-English account summary to stdout. With no backend it prints a clear message
// and exits 0 — it never crashes for lack of an LLM.
func runSummaryStdio(args []string) error {
	fs := flag.NewFlagSet("summary", flag.ContinueOnError)
	configPath := fs.String("config", summaryConfigPath(), "path to the BUCKS config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runSummaryFromConfig(
		summaryOutput,
		summaryBackends,
		*configPath,
		summaryPassphrase(),
		summarySecretOptions()...,
	)
}

var (
	summaryOutput        io.Writer       = os.Stdout
	summaryBackends      backendsFactory = envSummaryBackends
	summaryConfigPath                    = defaultConfigPath
	summaryPassphrase                    = passphraseFromEnv
	summarySecretOptions                 = func() []secrets.Option { return nil }
)

// snapshot is the account state a summary is built from: the Report plus the mode
// (paper/live) and halted flag. Production `bucks summary` builds this from the
// saved setup plus durable halt state, never from demo account numbers.
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
	return runSummaryWithBackends(out, backends, snap)
}

func runSummaryFromConfig(out io.Writer, newBackends backendsFactory, configPath, passphrase string, secretOpts ...secrets.Option) error {
	backends, err := newBackends()
	if err != nil {
		return fmt.Errorf("summary: %w", err)
	}
	if len(backends) == 0 {
		fmt.Fprint(out, noBackendMessage("summary"))
		return nil
	}
	snap, err := loadSummarySnapshot(configPath, passphrase, secretOpts...)
	if err != nil {
		return fmt.Errorf("summary: load saved setup: %w", err)
	}
	return runSummaryWithBackends(out, backends, snap)
}

func runSummaryWithBackends(out io.Writer, backends []analyst.Backend, snap snapshot) error {
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

func loadSummarySnapshot(configPath, passphrase string, secretOpts ...secrets.Option) (snapshot, error) {
	r, err := LoadSetup(configPath, passphrase, secretOpts...)
	if err != nil {
		return snapshot{}, err
	}
	snap := initialSnapshot(r)
	halted, err := loadSummaryHaltState(configPath)
	if err != nil {
		return snapshot{}, err
	}
	return snapshot{
		report: snap.Report,
		mode:   "paper",
		halted: halted,
	}, nil
}

func loadSummaryHaltState(configPath string) (bool, error) {
	ks, err := risk.Open(filepath.Join(filepath.Dir(configPath), "killswitch.json"))
	if err != nil {
		return false, err
	}
	halted, _ := ks.IsHalted()
	return halted, nil
}

// demoSnapshot is a small, explicit example fixture for tests and demos. Production
// summary output loads the owner's saved setup instead of presenting example numbers.
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
