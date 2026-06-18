package tui

import (
	"regexp"
	"strings"
	"testing"
)

// stepOfNRe captures any "step N of M" locator the header might render so the
// test can assert on the numbers rather than a brittle exact string.
var stepOfNRe = regexp.MustCompile(`step (\d+) of (\d+)`)

// realSteps are the six steps that are genuine setup work (Welcome..Safety).
// StepDone is the terminal "complete" state and is deliberately excluded.
var realSteps = []Step{StepWelcome, StepTelegram, StepLLM, StepBroker, StepIntake, StepSafety}

// TestHeaderNeverShowsStepSevenOfSix walks every step in stepOrder, renders the
// header, and asserts the counter is sane on every screen — in particular the
// final Done screen must NOT read "step 7 of 6".
func TestHeaderNeverShowsStepSevenOfSix(t *testing.T) {
	for _, s := range stepOrder {
		m := NewWizard()
		m.step = s
		header := firstLine(m.View())

		// Assertion 1: never an out-of-range "step N of 6" with N > 6
		// (and certainly never the reported "step 7 of 6").
		if strings.Contains(header, "step 7 of 6") {
			t.Errorf("step %s: header contains forbidden 'step 7 of 6': %q", s, header)
		}
		if mt := stepOfNRe.FindStringSubmatch(header); mt != nil {
			n := mustAtoi(t, mt[1])
			total := mustAtoi(t, mt[2])
			if total != 6 {
				t.Errorf("step %s: header total = %d, want 6: %q", s, total, header)
			}
			if n > total {
				t.Errorf("step %s: header step number %d exceeds total %d: %q", s, n, total, header)
			}
		}
	}
}

// TestRealStepsNumberedOneThroughSix asserts each of the six genuine setup steps
// shows "step K of 6" with K matching its 1-based position.
func TestRealStepsNumberedOneThroughSix(t *testing.T) {
	for i, s := range realSteps {
		want := i + 1 // 1-based
		m := NewWizard()
		m.step = s
		header := firstLine(m.View())

		mt := stepOfNRe.FindStringSubmatch(header)
		if mt == nil {
			t.Fatalf("step %s: header has no 'step N of M' locator: %q", s, header)
		}
		got := mustAtoi(t, mt[1])
		total := mustAtoi(t, mt[2])
		if got != want || total != 6 {
			t.Errorf("step %s: header = 'step %d of %d', want 'step %d of 6': %q", s, got, total, want, header)
		}
	}
}

// TestDoneScreenShowsCompletionNotStepSeven asserts the terminal screen renders a
// clean completion locator and never a numbered step.
func TestDoneScreenShowsCompletionNotStepSeven(t *testing.T) {
	m := NewWizard()
	m.step = StepDone
	header := firstLine(m.View())

	if strings.Contains(header, "step 7") {
		t.Errorf("Done header contains 'step 7': %q", header)
	}
	if stepOfNRe.MatchString(header) {
		t.Errorf("Done header still renders a numbered 'step N of M' locator: %q", header)
	}
	lower := strings.ToLower(header)
	if !strings.Contains(lower, "complete") && !strings.Contains(lower, "done") {
		t.Errorf("Done header has no completion locator (want 'complete'/'done'): %q", header)
	}
}

// firstLine returns the header line (the first rendered line) of a View(), with
// any trailing whitespace trimmed.
func firstLine(view string) string {
	if idx := strings.IndexByte(view, '\n'); idx >= 0 {
		return strings.TrimRight(view[:idx], " \t")
	}
	return strings.TrimRight(view, " \t")
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("non-numeric digit in %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}
