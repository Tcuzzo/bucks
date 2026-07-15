package tui

import (
	"strings"
	"testing"
)

// TestCoinbaseAndTradierReachLiveArmStep proves the NON-Alpaca real-money venues get the
// SAME live-arm treatment as alpaca-live: selecting Coinbase or Tradier must reach the
// safety step with the explicit live toggle OFFERED, the 'l' toggle must arm live (not be
// refused as "needs a live broker"), and the finished SetupResult must carry Live=true.
// An order at either venue moves actual funds — the wizard must never treat them as paper.
func TestCoinbaseAndTradierReachLiveArmStep(t *testing.T) {
	cases := []struct {
		pick string
		kind BrokerKind
	}{
		{"3", BrokerCoinbase},
		{"4", BrokerTradier},
	}
	for _, c := range cases {
		t.Run(string(c.kind), func(t *testing.T) {
			m := NewWizard()
			m, _ = send(t, m, enter()) // welcome -> telegram
			m = typeString(t, m, "123456789:AAH-validlookingtoken")
			m, _ = send(t, m, enter()) // telegram -> llm
			m = typeString(t, m, "1")  // OAuth — no key sub-prompt
			m, _ = send(t, m, enter()) // llm -> broker
			m = typeString(t, m, c.pick)
			if m.brokerKind != c.kind {
				t.Fatalf("picking %q selected %q, want %q", c.pick, m.brokerKind, c.kind)
			}
			m = typeString(t, m, "REALKEY-abcdef12345")
			m, _ = send(t, m, enter()) // broker key -> secret sub-prompt
			m = typeString(t, m, "REALSECRET-uvwxyz67890")
			m, _ = send(t, m, enter()) // broker -> intake
			m = answerIntake(t, m)     // intake -> safety
			if m.CurrentStep() != StepSafety {
				t.Fatalf("after intake: %v, want StepSafety", m.CurrentStep())
			}

			// The live-arm confirmation must be OFFERED — this venue trades real money.
			if view := m.safetyView(); !strings.Contains(view, "[l] toggle LIVE/paper") {
				t.Errorf("%s is a real-money venue: the safety step must OFFER the live-arm toggle; view:\n%s", c.kind, view)
			}

			// The explicit 'l' toggle must ARM live, never refuse with "needs a live broker".
			m = typeString(t, m, "l")
			if m.errMsg != "" {
				t.Fatalf("toggling live on %s was refused: %q", c.kind, m.errMsg)
			}
			if !m.live {
				t.Fatalf("live not armed on %s after the explicit toggle", c.kind)
			}
			if view := m.safetyView(); !strings.Contains(view, "LIVE — real money") {
				t.Errorf("armed %s must show the LIVE (real money) mode; view:\n%s", c.kind, view)
			}

			m, _ = send(t, m, enter()) // safety -> done
			if !m.Done() {
				t.Fatalf("wizard not Done after arming %s", c.kind)
			}
			res := m.Result()
			if res.Brokers[0].Kind != c.kind {
				t.Errorf("result broker = %q, want %q", res.Brokers[0].Kind, c.kind)
			}
			if !res.Live {
				t.Errorf("SetupResult.Live = false, want true — the armed %s selection was dropped", c.kind)
			}
		})
	}
}
