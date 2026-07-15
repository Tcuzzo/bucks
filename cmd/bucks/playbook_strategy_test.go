package main

import (
	"context"
	"strings"
	"testing"

	"bucks/internal/analyst"
	"bucks/internal/brokers"
	"bucks/internal/brokers/mock"
	"bucks/internal/orders"
	"bucks/internal/playbook"
	"bucks/internal/risk"
	"bucks/internal/tui"
)

// fakeBrain is a hermetic analyst.Backend returning a canned lean line, so the playbook decider
// is tested with NO network and NO real LLM.
type fakeBrain struct{ out string }

func (f fakeBrain) Name() string                                     { return "fake-brain" }
func (f fakeBrain) Complete(context.Context, string) (string, error) { return f.out, nil }

func aggressiveTechPlaybook(t *testing.T) playbook.Playbook {
	t.Helper()
	pb, err := playbook.BuildPlaybook(map[string]string{
		playbook.KeyRiskTolerance: "aggressive",
		playbook.KeyCapital:       "100000",
		playbook.KeyStyle:         "swing",
		playbook.KeySectors:       "tech",
		playbook.KeyMaxDrawdown:   "0.20",
	})
	if err != nil {
		t.Fatalf("playbook: %v", err)
	}
	return pb
}

// TestPlaybookUniverseFromSectors proves the BOT builds its universe from the playbook sectors
// (not the operator), and falls back to a liquid default when sectors don't map.
func TestPlaybookUniverseFromSectors(t *testing.T) {
	pb := aggressiveTechPlaybook(t)
	u := playbookUniverse(pb)
	if len(u) == 0 || u[0] != "AAPL" {
		t.Errorf("tech sector should yield a tech universe, got %v", u)
	}

	var none playbook.Playbook
	if got := playbookUniverse(none); len(got) == 0 {
		t.Error("a playbook with no sectors must still yield a default universe")
	}
}

// TestPlaybookDeciderBullishEntrySizedFromPlaybook is the core proof: given the owner's
// AGGRESSIVE/tech playbook and a bullish brain, the bot picks a tech symbol itself and sizes a
// long entry from the playbook's OWN risk budget — the operator does no work. equity 100000 *
// 2% risk = $2000 budget; aggressive stop = 5% so stop=95 on a 100 ask, $5 risk/share ->
// qty = 2000/5 = 400.
func TestPlaybookDeciderBullishEntrySizedFromPlaybook(t *testing.T) {
	pb := aggressiveTechPlaybook(t)
	an, err := analyst.New(pb, nil, fakeBrain{out: "LEAN: bullish\nStrong uptrend."})
	if err != nil {
		t.Fatalf("analyst: %v", err)
	}
	b := mock.New()
	b.SetQuote(brokers.Quote{Symbol: "AAPL", Bid: dec(t, "99"), Ask: dec(t, "100"), Last: dec(t, "100")})

	d := newPlaybookDecider(an, b, pb)
	snap := AccountSnapshot{
		Equity: dec(t, "100000"), Cash: dec(t, "100000"),
		Portfolio: risk.PortfolioState{Positions: map[string]risk.HeldPosition{}, OpenPositionCount: -1},
	}

	dec0 := d.Decide(context.Background(), snap)
	if !dec0.HasProposal {
		t.Fatalf("bullish brain on a playbook symbol must propose a trade; reason: %q", dec0.Reason)
	}
	p := dec0.Proposal
	if p.Symbol != "AAPL" || p.Side != orders.SideBuy {
		t.Errorf("expected a BUY of AAPL, got %s %s", p.Side, p.Symbol)
	}
	if p.StopPx.Cmp(p.EntryPx) >= 0 {
		t.Errorf("long entry stop %s must be BELOW entry %s", p.StopPx, p.EntryPx)
	}
	if p.Qty.Cmp(dec(t, "400")) != 0 {
		t.Errorf("qty = %s, want 400 (sized from the playbook's risk budget)", p.Qty)
	}
	if !strings.Contains(dec0.Reason, "playbook brain") {
		t.Errorf("reason should credit the playbook-driven brain, got %q", dec0.Reason)
	}
}

// TestPlaybookDeciderNeutralHolds proves a non-bullish brain holds (no forced trade).
func TestPlaybookDeciderNeutralHolds(t *testing.T) {
	pb := aggressiveTechPlaybook(t)
	an, _ := analyst.New(pb, nil, fakeBrain{out: "LEAN: neutral\nNo clear edge."})
	b := mock.New()
	b.SetQuote(brokers.Quote{Symbol: "AAPL", Bid: dec(t, "99"), Ask: dec(t, "100"), Last: dec(t, "100")})
	d := newPlaybookDecider(an, b, pb)
	got := d.Decide(context.Background(), AccountSnapshot{Equity: dec(t, "100000"),
		Portfolio: risk.PortfolioState{Positions: map[string]risk.HeldPosition{}, OpenPositionCount: -1}})
	if got.HasProposal {
		t.Errorf("a neutral brain must Hold, got a proposal: %+v", got.Proposal)
	}
}

// TestPlaybookDeciderAlreadyLongHolds proves the bot never stacks a second long on a held
// symbol (long-only v1 over-concentration guard, on top of the risk engine's limits).
func TestPlaybookDeciderAlreadyLongHolds(t *testing.T) {
	pb := aggressiveTechPlaybook(t)
	an, _ := analyst.New(pb, nil, fakeBrain{out: "LEAN: bullish\nUp."})
	b := mock.New()
	b.SetQuote(brokers.Quote{Symbol: "AAPL", Bid: dec(t, "99"), Ask: dec(t, "100"), Last: dec(t, "100")})
	d := newPlaybookDecider(an, b, pb)
	snap := AccountSnapshot{Equity: dec(t, "100000"),
		Portfolio: risk.PortfolioState{
			Positions:         map[string]risk.HeldPosition{"AAPL": {Qty: dec(t, "10"), MarkPx: dec(t, "100")}},
			OpenPositionCount: -1,
		}}
	if got := d.Decide(context.Background(), snap); got.HasProposal {
		t.Errorf("must Hold when already long AAPL, got a proposal: %+v", got.Proposal)
	}
}

// TestBuildDeciderUsesPlaybookBrainWhenConfigured proves that when a brain is configured, the
// loop wires the PLAYBOOK-DRIVEN decider (the bot trades the playbook), not monitor-only.
func TestBuildDeciderUsesPlaybookBrainWhenConfigured(t *testing.T) {
	prev := codexAvailable
	codexAvailable = func() bool { return true } // a brain is available (ChatGPT via codex)
	defer func() { codexAvailable = prev }()

	r := tui.SetupResult{LLM: tui.LLMOAuthGPT, Playbook: aggressiveTechPlaybook(t)}
	d := buildDecider(r, mock.New(), func(string, ...any) {})
	if _, ok := d.(*playbookDecider); !ok {
		t.Errorf("with a brain configured, expected the playbook-driven decider, got %T", d)
	}
}

// TestBuildDeciderMonitorOnlyWithoutBrain proves that with NO brain, the loop safely falls back
// to monitor-only (it watches + protects, but never invents a trade).
func TestBuildDeciderMonitorOnlyWithoutBrain(t *testing.T) {
	prev := codexAvailable
	codexAvailable = func() bool { return false } // no codex; OAuth-GPT yields no backend
	defer func() { codexAvailable = prev }()
	t.Setenv("BUCKS_CHAT_BASEURL", "")
	t.Setenv("BUCKS_CHAT_KEY", "")
	t.Setenv("BUCKS_CHAT_PROVIDER", "")

	r := tui.SetupResult{LLM: tui.LLMOAuthGPT, Playbook: aggressiveTechPlaybook(t)}
	d := buildDecider(r, mock.New(), func(string, ...any) {})
	if _, ok := d.(*playbookDecider); ok {
		t.Error("with no brain, expected monitor-only, got the playbook decider")
	}
}
