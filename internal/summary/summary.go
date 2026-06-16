// Package summary is BUCKS's plain-English surface: it turns the trader's real
// state (P&L, positions, cash, halted?, mode) into a short summary a first-timer
// can read — "Here's where you stand today..." — and it does so HONESTLY. The
// numbers in the summary are not the model's to invent: they are GROUNDED against
// the real Report the same way the chat surface grounds account facts.
//
// Two invariants carry in by construction, both reused (never re-implemented):
//
//   - NO FABRICATED ACCOUNT NUMBERS (GHOST honesty). The real facts are rendered
//     into the prompt straight off the Report (exact Decimal text, never float64),
//     so the model is handed the truth. After it replies, any number it states as
//     the owner's account state is run through analyst.Ground — the ONE honesty
//     engine the analyst and chat already use. A figure that matches a real fact
//     grounds VERIFIED; a fabricated figure (e.g. "+999.99" with no fact to back
//     it) is FLAGGED unverified, never presented as truth. We never write a third
//     grounding implementation — we feed the same account-fact extraction into the
//     same analyst.Ground.
//   - NO SILENT MODEL DOWNGRADE. Summaries run over an ORDERED list of
//     analyst.Backends with the same failover discipline as the analyst/chat:
//     when the primary errors the next backend answers and that downgrade is
//     RECORDED (in the returned Account summary's Failovers trail and on a Logger)
//     — never a quiet swap.
//
// Nothing here makes a network call in the default suite: a Backend is the thin
// analyst.Complete(ctx, prompt) contract driven by mocks. A real-model path exists
// only behind the `summary_live` build tag.
package summary

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"bucks/internal/analyst"
	"bucks/internal/channel"
	"bucks/internal/orders"
)

// AccountSummary is the grounded result of SummarizeAccount: the plain-English
// prose the model wrote, the backend that produced it (after any failover), the
// failover trail (empty when the primary answered — visible, never silent), and
// the fact-bearing claims found in the prose, each grounded against the REAL
// facts. A non-empty Unverified set means the summary stated a number BUCKS could
// not back — surfaced as unverified, never as established fact.
type AccountSummary struct {
	// Text is the model's plain-English summary, as written.
	Text string
	// Backend is the model that PRODUCED the summary (surviving backend after any
	// failover).
	Backend string
	// Failovers records every downgrade on the way to Backend (no-silent-downgrade).
	Failovers []analyst.Failover
	// Claims are the account figures stated in the summary, grounded against the
	// real facts (Verified=true only when a real fact backs the number).
	Claims []analyst.Claim
}

// Downgraded reports whether the summary was produced after at least one failover.
func (s AccountSummary) Downgraded() bool { return len(s.Failovers) > 0 }

// Unverified returns the summary's claims that did NOT ground against real facts —
// the numbers BUCKS could not back. A non-empty result must be shown as unverified.
func (s AccountSummary) Unverified() []analyst.Claim {
	var out []analyst.Claim
	for _, c := range s.Claims {
		if !c.Verified {
			out = append(out, c)
		}
	}
	return out
}

// SummarizeAccount hands the model the REAL account facts off the Report (exact
// Decimal text — never float64) and asks for a short, plain-English summary. It
// reasons over an ORDERED list of backends (primary first) with VISIBLE failover.
// After the model replies, the prose is run through accountGround (which feeds the
// stated account numbers into the single analyst.Ground honesty engine) so a
// fabricated figure is FLAGGED, not presented as truth. If EVERY backend fails it
// returns a clear joined error and fabricates nothing.
//
// rep is the harness/channel Report (the P&L/position snapshot already built);
// mode is "paper"/"live"; halted reports whether the trader is currently halted.
// These render into the prompt as the model's only source of account truth.
func SummarizeAccount(ctx context.Context, backends []analyst.Backend, rep channel.Report, mode string, halted bool, opts ...Option) (AccountSummary, error) {
	if len(backends) == 0 {
		return AccountSummary{}, errors.New("summary: at least one backend is required")
	}
	cfg := newConfig(opts...)

	facts := accountFacts(rep, mode, halted)
	prompt := buildAccountPrompt(rep, facts)

	var failovers []analyst.Failover
	var errs []error
	for i, b := range backends {
		out, err := b.Complete(ctx, prompt)
		if err != nil {
			fo := analyst.Failover{From: b.Name(), Err: err.Error()}
			failovers = append(failovers, fo)
			errs = append(errs, fmt.Errorf("backend %s: %w", b.Name(), err))
			if i+1 < len(backends) {
				cfg.log.Printf("summary: backend %q failed (%v) — failing over to %q",
					b.Name(), err, backends[i+1].Name())
			} else {
				cfg.log.Printf("summary: backend %q failed (%v) — no further backends",
					b.Name(), err)
			}
			continue
		}
		text := strings.TrimSpace(out)
		// Ground any account figures the model stated against the REAL facts, reusing
		// the single analyst.Ground honesty engine (no third grounding implementation).
		claims := accountGround(text, facts)
		if len(failovers) > 0 {
			cfg.log.Printf("summary: produced summary on fallback backend %q after %d failover(s)",
				b.Name(), len(failovers))
		}
		return AccountSummary{
			Text:      text,
			Backend:   b.Name(),
			Failovers: failovers,
			Claims:    claims,
		}, nil
	}
	return AccountSummary{}, fmt.Errorf("summary: all %d backend(s) failed: %w",
		len(backends), errors.Join(errs...))
}

// accountFacts renders the Report into the fact-key -> value map that is BOTH the
// model's only source of account truth (shown in the prompt) AND the grounding
// evidence (the real numbers a stated figure must match). Every money value is the
// exact Decimal string (never float64). Keys are stable and human-readable so the
// model can echo them and the cue detection recognizes them.
func accountFacts(rep channel.Report, mode string, halted bool) map[string]string {
	facts := map[string]string{
		"equity":         rep.Equity.String(),
		"realized_pnl":   rep.RealizedPL.String(),
		"unrealized_pnl": rep.UnrealizedPL.String(),
		"mode":           modeText(mode),
		"halted":         boolText(halted),
	}
	// Per-position facts (qty + unrealized P&L) keyed by symbol, exact Decimal.
	for _, p := range rep.Positions {
		sym := strings.ToLower(strings.TrimSpace(p.Symbol))
		if sym == "" {
			continue
		}
		facts[sym+"_qty"] = p.Qty.String()
		facts[sym+"_unrealized_pnl"] = p.UnrealizedPL.String()
	}
	return facts
}

// modeText normalizes the trading mode to a clear word; an unknown/empty mode is
// reported as "unknown" rather than guessed.
func modeText(mode string) string {
	m := strings.ToLower(strings.TrimSpace(mode))
	switch m {
	case "paper", "live":
		return m
	case "":
		return "unknown"
	default:
		return m
	}
}

func boolText(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// buildAccountPrompt composes the plain-English-summary prompt. The REAL facts are
// rendered EXACTLY (Decimal text), and the model is told these are the only account
// numbers it may state — the same honesty contract chat uses. Deterministic given
// identical inputs.
func buildAccountPrompt(rep channel.Report, facts map[string]string) string {
	var b strings.Builder
	b.WriteString("You are BUCKS, an honest trading assistant. Write a SHORT, plain-English summary " +
		"a first-time investor can understand — start with \"Here's where you stand today...\".\n")
	b.WriteString("Be honest: NEVER promise profit, and make clear paper trading is not real money.\n")
	b.WriteString("State ONLY the account numbers given below — do NOT invent, round, or estimate any figure.\n")
	b.WriteString("ACCOUNT FACTS (the ONLY account numbers you may state as fact):\n")
	for _, k := range sortedKeys(facts) {
		fmt.Fprintf(&b, "- %s = %s\n", k, facts[k])
	}
	// Render the positions explicitly too, so the model sees them as a list (the
	// exact same exact-decimal values that are in facts).
	if len(rep.Positions) > 0 {
		b.WriteString("OPEN POSITIONS:\n")
		positions := make([]channel.Position, len(rep.Positions))
		copy(positions, rep.Positions)
		sort.Slice(positions, func(i, j int) bool { return positions[i].Symbol < positions[j].Symbol })
		for _, p := range positions {
			fmt.Fprintf(&b, "- %s: qty %s, mark %s, unrealized P&L %s\n",
				p.Symbol, p.Qty.String(), p.MarkPx.String(), p.UnrealizedPL.String())
		}
	} else {
		b.WriteString("OPEN POSITIONS: none.\n")
	}
	b.WriteString("Now write the summary:")
	return b.String()
}

// Summarize is the generic plain-English summarizer (slice 15's research surface
// will reuse this): summarize arbitrary text per an instruction ("summarize for a
// beginner", "3 bullet points"). It reasons over the ORDERED backends with VISIBLE
// failover and returns the reply plus the failover trail. Empty text or no backend
// is reported as a clear error (never a panic), and a total backend failure returns
// a clear joined error — nothing is fabricated.
func Summarize(ctx context.Context, backends []analyst.Backend, text, instruction string) (AdHocSummary, error) {
	if len(backends) == 0 {
		return AdHocSummary{}, errors.New("summary: at least one backend is required")
	}
	if strings.TrimSpace(text) == "" {
		return AdHocSummary{}, errors.New("summary: nothing to summarize (empty text)")
	}
	prompt := buildAdHocPrompt(text, instruction)

	var failovers []analyst.Failover
	var errs []error
	for _, b := range backends {
		out, err := b.Complete(ctx, prompt)
		if err != nil {
			failovers = append(failovers, analyst.Failover{From: b.Name(), Err: err.Error()})
			errs = append(errs, fmt.Errorf("backend %s: %w", b.Name(), err))
			continue
		}
		return AdHocSummary{
			Text:      strings.TrimSpace(out),
			Backend:   b.Name(),
			Failovers: failovers,
		}, nil
	}
	return AdHocSummary{}, fmt.Errorf("summary: all %d backend(s) failed: %w",
		len(backends), errors.Join(errs...))
}

// AdHocSummary is the result of Summarize: the model's plain-English summary, the
// backend that produced it, and the failover trail (empty when the primary
// answered — visible, never silent). There is no account grounding here: the input
// is arbitrary text, not the owner's account state.
type AdHocSummary struct {
	Text      string
	Backend   string
	Failovers []analyst.Failover
}

// Downgraded reports whether the ad-hoc summary came from a fallback backend.
func (s AdHocSummary) Downgraded() bool { return len(s.Failovers) > 0 }

// buildAdHocPrompt composes the generic-summary prompt: the instruction (with a
// plain-English default) plus the source text. Deterministic given identical input.
func buildAdHocPrompt(text, instruction string) string {
	instr := strings.TrimSpace(instruction)
	if instr == "" {
		instr = "Summarize the following in plain English a first-timer can understand."
	}
	var b strings.Builder
	b.WriteString("You are BUCKS, an honest assistant. ")
	b.WriteString(instr)
	b.WriteString("\nDo NOT add facts that are not in the text; if it is unclear, say so plainly.\n")
	b.WriteString("TEXT TO SUMMARIZE:\n")
	b.WriteString(text)
	b.WriteString("\nNow write the summary:")
	return b.String()
}

// sortedKeys gives a deterministic key order for the rendered facts block.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// compile-time assertion that we depend on the money type, never float64, for the
// figures we render — keeps the import honest and the seam typed.
var _ orders.Decimal = orders.ZeroDecimal
