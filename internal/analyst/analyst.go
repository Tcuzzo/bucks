// Package analyst is BUCKS's playbook-driven LLM layer: it turns the owner's
// playbook plus a live market context into a typed View (a read of the market and
// a leaning), and it does so honestly. Two invariants from the spec are wired in
// by construction here:
//
//   - NO SILENT MODEL DOWNGRADE (§6). The analyst runs over one or more Backends
//     with FAILOVER. Every failover — every drop from the primary to a weaker
//     backend — is RECORDED in the View and on a logger, never a quiet swap. The
//     View names exactly which backend produced it.
//   - NO FABRICATED EDGE / NO FAKE BACKTEST (§4.5, GHOST honesty). Analyze takes
//     the supporting evidence and runs the View's claims through Ground(view,
//     evidence) BEFORE returning, so every View that leaves Analyze is already
//     grounded (Grounded=true) with each claim labeled verified/unverified. A claim
//     not backed by real supplied evidence comes back Unverified — never presented
//     as fact. Passing nil/empty evidence marks ALL claims unverified (it never
//     silently verifies an ungrounded claim).
//
// The package is backend-agnostic: a Backend is a thin Complete(ctx, prompt)
// contract, so OAuth-ChatGPT and an Ollama-cloud/MiniMax key are interchangeable
// and tested against httptest servers. A real-network path exists only behind the
// `analyst_live` build tag — never in the default suite.
package analyst

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"bucks/internal/playbook"
)

// Backend is a single LLM provider behind a uniform contract. Name() is the
// stable identifier recorded in the View and the failover log; Complete sends a
// fully-formed prompt and returns the model's text (or an error, which triggers
// failover). Implementations must not panic on a transport error — return it.
type Backend interface {
	Name() string
	Complete(ctx context.Context, prompt string) (string, error)
}

// Logger is the minimal sink the analyst writes failover notices to, so a
// downgrade is visible in the operator's logs as well as in the View. The
// standard library's log.Printf satisfies this; tests pass a capturing logger.
type Logger interface {
	Printf(format string, args ...any)
}

// nopLogger is the default when no logger is supplied: failovers still land in
// the View (the structured, asserted record); the logger is the human-facing echo.
type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// MarketContext is the live read the analyst reasons over. It is intentionally a
// plain, typed snapshot (no float64 money — Notes carries any figures as text the
// caller already grounded) so the analyst layer never invents a price.
type MarketContext struct {
	Symbol string
	// Summary is a plain-English description of the current tape (trend, range,
	// volatility regime) assembled by the caller from real indicators/data.
	Summary string
	// Notes are additional grounded facts (e.g. "ATR(14)=2.31", "RSI=68") the
	// caller has already computed. They are passed to the model as context only.
	Notes []string
}

// Lean is the analyst's directional leaning — deliberately advisory, NOT an
// order. The risk engine and strategy layer own actual order decisions; the View
// is an input, never an execution.
type Lean string

const (
	// LeanBullish — the analyst reads upside.
	LeanBullish Lean = "bullish"
	// LeanBearish — the analyst reads downside.
	LeanBearish Lean = "bearish"
	// LeanNeutral — no clear edge (the honest default).
	LeanNeutral Lean = "neutral"
)

// Failover records one drop from a failed backend to the next. It is part of the
// View so the downgrade is structured and asserted, not buried in a log line.
type Failover struct {
	// From is the backend that failed.
	From string
	// Err is the failure that triggered the failover (the real error string).
	Err string
}

// Claim is one assertion the analyst made that may carry a checkable fact (a
// number, a cited stat, an edge). After Ground, Verified reflects whether the
// claim was backed by real evidence. EvidenceKey is the evidence map key that was
// looked up (empty for a claim that carries no checkable fact).
type Claim struct {
	// Text is the human-readable claim.
	Text string
	// EvidenceKey, when non-empty, is the fact the claim depends on; Ground looks
	// it up in the evidence map.
	EvidenceKey string
	// Verified is set by Ground: true only when EvidenceKey resolved to matching
	// real evidence. A claim with an EvidenceKey that does not resolve is
	// Verified=false (unverified) — never presented as fact.
	Verified bool
}

// View is the analyst's typed output. Backend names the model that PRODUCED it
// (the surviving backend after any failover). Failovers lists every downgrade
// that happened on the way (empty when the primary answered) — this is the
// no-silent-downgrade record. Grounded is set by Ground; until then the View's
// claims are ungrounded and must not be treated as fact.
type View struct {
	Symbol    string
	Lean      Lean
	Rationale string
	Backend   string
	Failovers []Failover
	Claims    []Claim
	// Grounded is false until Ground() has run; a consumer must not present
	// claims as fact on an ungrounded View.
	Grounded bool
}

// Downgraded reports whether the View was produced after at least one failover —
// i.e. a weaker/alternate backend answered because the primary failed. True means
// the operator should see the degraded-routing notice; it is never hidden.
func (v View) Downgraded() bool { return len(v.Failovers) > 0 }

// UnverifiedClaims returns the claims that did NOT ground against real evidence.
// A non-empty result means the View contains assertions that must be shown as
// "unverified", never as established fact.
func (v View) UnverifiedClaims() []Claim {
	var out []Claim
	for _, c := range v.Claims {
		if !c.Verified {
			out = append(out, c)
		}
	}
	return out
}

// Analyst runs the playbook-driven analysis over an ordered list of backends with
// failover. The first backend is the primary; each subsequent one is a fallback
// used ONLY when the preceding backends errored, and every such use is recorded.
type Analyst struct {
	backends []Backend
	pb       playbook.Playbook
	log      Logger
}

// New builds an Analyst over the given playbook and an ORDERED list of backends
// (primary first). It requires at least one backend; an empty list is a
// programming error and is reported, not silently tolerated.
func New(pb playbook.Playbook, log Logger, backends ...Backend) (*Analyst, error) {
	if len(backends) == 0 {
		return nil, errors.New("analyst: at least one backend is required")
	}
	if log == nil {
		log = nopLogger{}
	}
	return &Analyst{backends: backends, pb: pb, log: log}, nil
}

// Analyze builds a prompt from the playbook + market context, calls the primary
// backend, and FAILS OVER to each next backend on error. The returned View names
// the surviving backend and lists every failover (so a downgrade is visible, not
// silent). If EVERY backend fails, it returns a clear error that wraps all the
// underlying failures — it never fabricates a View. The raw model text is parsed
// into a structured View; parsing is conservative (an unrecognized leaning maps
// to neutral, the honest default).
//
// evidence is the set of real, system-computed facts (fact-key -> value) the
// caller has actually run. Before returning, Analyze runs the parsed View through
// Ground(view, evidence), so the View that leaves IS grounded by construction
// (Grounded=true) and each fact-bearing claim is labeled verified ONLY when backed
// by matching real evidence. Passing nil/empty evidence is legal and marks ALL
// claims unverified — it never silently verifies an unsupported claim.
func (a *Analyst) Analyze(ctx context.Context, mc MarketContext, evidence map[string]string) (View, error) {
	prompt := a.buildPrompt(mc)

	var failovers []Failover
	var errs []error
	for i, b := range a.backends {
		out, err := b.Complete(ctx, prompt)
		if err != nil {
			// Record the downgrade in BOTH the structured trail and the log — this
			// is the explicit no-silent-downgrade wiring.
			fo := Failover{From: b.Name(), Err: err.Error()}
			failovers = append(failovers, fo)
			errs = append(errs, fmt.Errorf("backend %s: %w", b.Name(), err))
			if i+1 < len(a.backends) {
				a.log.Printf("analyst: backend %q failed (%v) — failing over to %q",
					b.Name(), err, a.backends[i+1].Name())
			} else {
				a.log.Printf("analyst: backend %q failed (%v) — no further backends",
					b.Name(), err)
			}
			continue
		}
		view := parseView(mc.Symbol, out)
		view.Backend = b.Name()
		view.Failovers = failovers
		if len(failovers) > 0 {
			a.log.Printf("analyst: produced View on fallback backend %q after %d failover(s)",
				b.Name(), len(failovers))
		}
		// Ground by construction: every View that leaves Analyze is grounded, so a
		// caller can never accidentally present ungrounded claims as fact. nil/empty
		// evidence => all claims come back Unverified (never silently verified).
		return Ground(view, evidence), nil
	}
	// Every backend failed — no fabrication, a clear joined error.
	return View{}, fmt.Errorf("analyst: all %d backend(s) failed: %w",
		len(a.backends), errors.Join(errs...))
}

// buildPrompt composes the analysis prompt from the owner's playbook (mandate,
// register, style, drawdown tolerance, goals) and the live market context. It is
// deterministic given identical inputs.
func (a *Analyst) buildPrompt(mc MarketContext) string {
	var b strings.Builder
	b.WriteString("You are BUCKS, an honest trading analyst. Read the market for the owner.\n")
	b.WriteString("OWNER PLAYBOOK:\n")
	fmt.Fprintf(&b, "- risk tolerance: %s\n", a.pb.RiskTolerance)
	fmt.Fprintf(&b, "- style: %s\n", a.pb.Style)
	fmt.Fprintf(&b, "- capital: %s\n", a.pb.Capital.String())
	fmt.Fprintf(&b, "- max drawdown tolerated: %s\n", a.pb.MaxDrawdownPct.String())
	if len(a.pb.Sectors) > 0 {
		fmt.Fprintf(&b, "- sectors of interest: %s\n", strings.Join(a.pb.Sectors, ", "))
	}
	if a.pb.Goals != "" {
		fmt.Fprintf(&b, "- goals: %s\n", a.pb.Goals)
	}
	b.WriteString("MARKET CONTEXT:\n")
	fmt.Fprintf(&b, "- symbol: %s\n", mc.Symbol)
	fmt.Fprintf(&b, "- summary: %s\n", mc.Summary)
	for _, n := range mc.Notes {
		fmt.Fprintf(&b, "- note: %s\n", n)
	}
	b.WriteString("Respond with a leaning (bullish|bearish|neutral) and a short rationale. " +
		"Do NOT invent numbers or claim a backtested edge you did not run.\n")
	return b.String()
}
