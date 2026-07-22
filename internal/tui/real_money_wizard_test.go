package tui

import (
	"strings"
	"testing"
)

func TestBrokerViewOffersPaperOnly(t *testing.T) {
	m := NewWizard()
	m.step = StepBroker
	view := strings.ToLower(m.View())
	for _, forbidden := range []string{"coinbase", "alpaca — live", "tradier", "[1-3]"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("paper-only broker wizard advertises %q:\n%s", forbidden, view)
		}
	}
	if !strings.Contains(view, "alpaca — paper") || !strings.Contains(view, "bucks cannot trade real money") {
		t.Fatalf("broker wizard must plainly describe its paper-only limit:\n%s", view)
	}
}

func TestBrokerWizardRejectsFormerRealMoneyChoices(t *testing.T) {
	for _, pick := range []string{"2", "3"} {
		m := NewWizard()
		m.step = StepBroker
		m = typeString(t, m, pick)
		if m.brokerKind != BrokerAlpacaPaper || !strings.Contains(strings.ToLower(m.Err()), "cannot trade real money") {
			t.Fatalf("choice %s was not rejected clearly: broker=%q err=%q", pick, m.brokerKind, m.Err())
		}
	}
}
