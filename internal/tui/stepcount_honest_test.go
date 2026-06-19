package tui

import (
	"strings"
	"testing"
)

// headerLine returns the first rendered line of the wizard view (the locator header), so a
// test can assert what the header — not the body — tells the owner.
func headerLine(v string) string {
	if i := strings.IndexByte(v, '\n'); i >= 0 {
		return v[:i]
	}
	return v
}

// TestWizardHeaderShowsIntakeSubProgress proves the header no longer HIDES the 10-question
// intake under a frozen "step 5 of 6": it surfaces the real sub-progress ("question 3 of 10")
// so the owner is never surprised by screens the advertised count concealed.
func TestWizardHeaderShowsIntakeSubProgress(t *testing.T) {
	m := NewWizard()
	m.step = StepIntake
	m.intakeIdx = 2 // the 3rd question (0-based)

	h := headerLine(m.View())
	if !strings.Contains(h, "step 5 of 6") {
		t.Errorf("intake header lost the section locator; header:\n%s", h)
	}
	if !strings.Contains(h, "question 3 of 10") {
		t.Errorf("intake header must surface the hidden sub-progress 'question 3 of 10'; header:\n%s", h)
	}
}

// TestWizardHeaderLabelsKeySubPrompts proves the two hidden masked sub-prompts (the LLM API
// key and the broker API secret) are now named in the header, so the owner knows these are
// extra screens within a step, not a frozen single screen.
func TestWizardHeaderLabelsKeySubPrompts(t *testing.T) {
	llm := NewWizard()
	llm.step = StepLLM
	llm.llm = LLMCloudKey
	llm.llmKeyPhase = true
	if h := headerLine(llm.View()); !strings.Contains(h, "· your API key") {
		t.Errorf("LLM key sub-prompt must be labeled in the header; header:\n%s", h)
	}

	br := NewWizard()
	br.step = StepBroker
	br.brokerKind = BrokerAlpacaLive
	br.brokerSecretPhase = true
	if h := headerLine(br.View()); !strings.Contains(h, "· your API secret") {
		t.Errorf("broker secret sub-prompt must be labeled in the header; header:\n%s", h)
	}
}
