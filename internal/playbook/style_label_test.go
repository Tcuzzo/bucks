package playbook

import (
	"strings"
	"testing"
)

// styleQuestion returns the trading-style intake question (helper for the label tests).
func styleQuestion(t *testing.T) Question {
	t.Helper()
	for _, q := range DefaultIntake().Questions {
		if q.Id == KeyStyle {
			return q
		}
	}
	t.Fatal("no style question in the intake")
	return Question{}
}

// TestIntakeStyleShowsHoldNotHodl proves the owner-facing trading-style choices read "hold"
// (plain English) and never the crypto-slang "hodl", which reads as a misspelling to a
// non-crypto owner. The internal canonical Style value stays "hodl" for storage/back-compat;
// only the label the owner is shown changes.
func TestIntakeStyleShowsHoldNotHodl(t *testing.T) {
	q := styleQuestion(t)
	joined := strings.ToLower(strings.Join(q.Options, " "))
	if !strings.Contains(joined, "hold") {
		t.Errorf("style choices must offer 'hold': %v", q.Options)
	}
	for _, o := range q.Options {
		if strings.EqualFold(o, "hodl") {
			t.Errorf("style choices must NOT show the misspelled 'hodl' to the owner: %v", q.Options)
		}
	}
}

// TestBuildPlaybookAcceptsHoldAndHodl proves the owner can pick the new "hold" label AND any
// legacy "hodl" answer still works — both canonicalize to the Hodl style, so saved configs
// and prior answers keep validating (no break for anyone who already chose "hodl").
func TestBuildPlaybookAcceptsHoldAndHodl(t *testing.T) {
	for _, in := range []string{"hold", "hodl", "HOLD", "Hodl"} {
		pb, err := BuildPlaybook(map[string]string{
			KeyRiskTolerance: "moderate",
			KeyCapital:       "10000",
			KeyStyle:         in,
			KeyMaxDrawdown:   "0.20", // >= 0.10 so no hodl/drawdown contradiction
		})
		if err != nil {
			t.Fatalf("style %q should be accepted: %v", in, err)
		}
		if pb.Style != Hodl {
			t.Errorf("style %q should canonicalize to Hodl (%q), got %q", in, Hodl, pb.Style)
		}
	}
}
