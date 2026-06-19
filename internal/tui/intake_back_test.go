package tui

import "testing"

// TestIntakeEscGoesBackOneQuestion proves esc during the intake rewinds ONE question (and
// restores that answer for editing) instead of jumping out of the whole 10-question section —
// the confusing back-navigation the owner hit. It mirrors the LLM-key / broker-secret
// sub-prompt back behavior.
func TestIntakeEscGoesBackOneQuestion(t *testing.T) {
	m := NewWizard()
	m.step = StepIntake
	m.intakeIdx = 3
	m.answers = map[string]string{m.intake.Questions[2].Id: "prior-answer"}

	m = m.back()

	if m.CurrentStep() != StepIntake {
		t.Errorf("esc mid-intake must STAY in intake, went to %v", m.CurrentStep())
	}
	if m.intakeIdx != 2 {
		t.Errorf("esc must rewind one question (3->2), got intakeIdx=%d", m.intakeIdx)
	}
	if m.input != "prior-answer" {
		t.Errorf("esc-back should restore the prior answer for editing, got input=%q", m.input)
	}
}

// TestIntakeEscAtFirstQuestionLeavesToBroker proves that only at the FIRST intake question does
// esc leave the section (back to the broker step) — so the owner can still exit the intake.
func TestIntakeEscAtFirstQuestionLeavesToBroker(t *testing.T) {
	m := NewWizard()
	m.step = StepIntake
	m.intakeIdx = 0

	m = m.back()

	if m.CurrentStep() != StepBroker {
		t.Errorf("esc at the first intake question should leave to the broker step, got %v", m.CurrentStep())
	}
}
