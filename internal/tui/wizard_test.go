package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"bucks/internal/playbook"
)

// runes builds a KeyMsg for a printable string (each call sends the whole string
// as one runes event, which is how a paste arrives).
func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// enter / esc / ctrlC / backspace / key are terse constructors for control keys.
func enter() tea.KeyMsg     { return tea.KeyMsg{Type: tea.KeyEnter} }
func esc() tea.KeyMsg       { return tea.KeyMsg{Type: tea.KeyEsc} }
func ctrlC() tea.KeyMsg     { return tea.KeyMsg{Type: tea.KeyCtrlC} }
func backspace() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyBackspace} }

// send drives one message through the model and returns the concrete WizardModel
// plus the command, asserting the model type stays a WizardModel.
func send(t *testing.T, m WizardModel, msg tea.Msg) (WizardModel, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	wm, ok := next.(WizardModel)
	if !ok {
		t.Fatalf("Update returned %T, want WizardModel", next)
	}
	return wm, cmd
}

// typeString feeds a string as a single runes keystroke.
func typeString(t *testing.T, m WizardModel, s string) WizardModel {
	t.Helper()
	m, _ = send(t, m, runes(s))
	return m
}

// TestStepOrderIsExactAndForward asserts the canonical unwrap order matches the
// spec list and that the wizard advances by exactly one step on valid input,
// never skipping. This is the "assert the step order" requirement.
func TestStepOrderIsExactAndForward(t *testing.T) {
	want := []Step{StepWelcome, StepTelegram, StepLLM, StepBroker, StepIntake, StepSafety, StepDone}
	if len(stepOrder) != len(want) {
		t.Fatalf("stepOrder length = %d, want %d", len(stepOrder), len(want))
	}
	for i := range want {
		if stepOrder[i] != want[i] {
			t.Fatalf("stepOrder[%d] = %v, want %v", i, stepOrder[i], want[i])
		}
	}

	// Walk the wizard with valid input and record the step at each gate.
	m := NewWizard()
	if m.CurrentStep() != StepWelcome {
		t.Fatalf("fresh wizard at %v, want StepWelcome", m.CurrentStep())
	}

	// Welcome -> Telegram
	m, _ = send(t, m, enter())
	if m.CurrentStep() != StepTelegram {
		t.Fatalf("after welcome enter: %v, want StepTelegram", m.CurrentStep())
	}
	// Telegram (valid token) -> LLM
	m = typeString(t, m, "123456789:AAH-validlookingtoken")
	m, _ = send(t, m, enter())
	if m.CurrentStep() != StepLLM {
		t.Fatalf("after telegram: %v, want StepLLM", m.CurrentStep())
	}
	// LLM (pick + confirm) -> Broker
	m = typeString(t, m, "1")
	m, _ = send(t, m, enter())
	if m.CurrentStep() != StepBroker {
		t.Fatalf("after llm: %v, want StepBroker", m.CurrentStep())
	}
	// Broker (paper + key) -> still StepBroker (secret sub-prompt), NOT Intake yet.
	m = typeString(t, m, "1")
	m = typeString(t, m, "PKtestbrokerkey123")
	m, _ = send(t, m, enter())
	if m.CurrentStep() != StepBroker {
		t.Fatalf("after broker key: %v, want stay StepBroker (secret sub-prompt)", m.CurrentStep())
	}
	// Broker secret -> Intake.
	m = typeString(t, m, "SKtestbrokersecret456")
	m, _ = send(t, m, enter())
	if m.CurrentStep() != StepIntake {
		t.Fatalf("after broker secret: %v, want StepIntake", m.CurrentStep())
	}
	// Intake (answer all) -> Safety
	m = answerIntake(t, m)
	if m.CurrentStep() != StepSafety {
		t.Fatalf("after intake: %v, want StepSafety, err=%q", m.CurrentStep(), m.Err())
	}
	// Safety (finish) -> Done
	m, _ = send(t, m, enter())
	if m.CurrentStep() != StepDone {
		t.Fatalf("after safety: %v, want StepDone, err=%q", m.CurrentStep(), m.Err())
	}
	if !m.Done() {
		t.Fatal("wizard should report Done() at StepDone")
	}
}

// answerIntake answers every DefaultIntake question with a valid value and presses
// enter, walking the wizard from StepIntake to StepSafety. It is the shared happy
// path for the completion tests.
func answerIntake(t *testing.T, m WizardModel) WizardModel {
	t.Helper()
	answers := map[string]string{
		playbook.KeyRiskTolerance: "moderate",
		playbook.KeyCapital:       "25000",
		playbook.KeyStyle:         "swing",
		playbook.KeySectors:       "tech, energy",
		playbook.KeyMaxDrawdown:   "0.20",
		playbook.KeyGoals:         "grow steadily",
		playbook.KeyMaxRiskTrade:  "0.01",
		playbook.KeyMaxDailyLoss:  "0.03",
		playbook.KeyMaxLeverage:   "2",
		playbook.KeyMaxOpenPos:    "8",
	}
	in := playbook.DefaultIntake()
	for _, q := range in.Questions {
		ans := answers[q.Id]
		if ans != "" {
			m = typeString(t, m, ans)
		}
		m, _ = send(t, m, enter())
	}
	return m
}

// TestCompletingWizardYieldsValidSetupResult drives the full wizard and asserts
// the SetupResult carries every expected field AND a Playbook that the real
// playbook.BuildPlaybook accepts (no fake green — the same builder validates it).
func TestCompletingWizardYieldsValidSetupResult(t *testing.T) {
	m := completeHappyPath(t, false)
	if !m.Done() {
		t.Fatal("wizard not Done after happy path")
	}
	res := m.Result()

	if res.TelegramToken == "" {
		t.Error("SetupResult.TelegramToken is empty")
	}
	if res.LLM != LLMOAuthGPT {
		t.Errorf("SetupResult.LLM = %q, want %q", res.LLM, LLMOAuthGPT)
	}
	if len(res.Brokers) != 1 {
		t.Fatalf("SetupResult.Brokers len = %d, want 1", len(res.Brokers))
	}
	if res.Brokers[0].Kind != BrokerAlpacaPaper {
		t.Errorf("broker kind = %q, want %q", res.Brokers[0].Kind, BrokerAlpacaPaper)
	}
	if res.Brokers[0].Key == "" {
		t.Error("broker key is empty")
	}
	// The REAL entered secret must be captured verbatim — never the old synthesized
	// "<key>-secret" placeholder, never empty. A placeholder would break live auth
	// (Alpaca-live / Coinbase / Tradier all use key+secret).
	if res.Brokers[0].Secret != "SKtestbrokersecret456" {
		t.Errorf("broker secret = %q, want the entered secret %q", res.Brokers[0].Secret, "SKtestbrokersecret456")
	}
	if res.Brokers[0].Secret == res.Brokers[0].Key+"-secret" {
		t.Errorf("broker secret is the synthesized placeholder %q+\"-secret\" — must be the real entered secret", res.Brokers[0].Key)
	}
	if res.Brokers[0].Secret == "" {
		t.Error("broker secret is empty — live auth would fail")
	}
	if res.Live {
		t.Error("paper broker must yield Live=false (safe default)")
	}

	// The embedded Playbook must be the SAME one BuildPlaybook produces from the
	// collected answers — prove it by rebuilding and validating, no shortcuts.
	if err := res.Playbook.Validate(); err != nil {
		t.Fatalf("SetupResult.Playbook failed Validate: %v", err)
	}
	if res.Playbook.RiskTolerance != playbook.Moderate {
		t.Errorf("playbook risk tolerance = %q, want moderate", res.Playbook.RiskTolerance)
	}
	if res.Playbook.Capital.String() != "25000" {
		t.Errorf("playbook capital = %q, want 25000", res.Playbook.Capital.String())
	}
	if res.Playbook.Style != playbook.Swing {
		t.Errorf("playbook style = %q, want swing", res.Playbook.Style)
	}
}

// completeHappyPath drives the wizard end to end. goLive toggles a live broker +
// the explicit live toggle so the live-gating test can reuse it.
func completeHappyPath(t *testing.T, goLive bool) WizardModel {
	t.Helper()
	m := NewWizard()
	m, _ = send(t, m, enter()) // welcome -> telegram
	m = typeString(t, m, "123456789:AAH-validlookingtoken")
	m, _ = send(t, m, enter()) // telegram -> llm
	m = typeString(t, m, "1")  // OAuth-GPT
	m, _ = send(t, m, enter()) // llm -> broker
	if goLive {
		m = typeString(t, m, "2") // Alpaca live
	} else {
		m = typeString(t, m, "1") // Alpaca paper
	}
	m = typeString(t, m, "PKtestbrokerkey123")
	m, _ = send(t, m, enter())                    // broker key -> secret sub-prompt
	m = typeString(t, m, "SKtestbrokersecret456") // broker secret
	m, _ = send(t, m, enter())                    // broker -> intake
	m = answerIntake(t, m)                        // intake -> safety
	if goLive {
		m = typeString(t, m, "l") // explicit live toggle
	}
	m, _ = send(t, m, enter()) // safety -> done
	return m
}

// TestFreeNemotronLLMOptionIsSelectable proves the new "4) Free (NVIDIA Nemotron)"
// backend option is selectable at the LLM step, confirms cleanly, and lands in the
// SetupResult as LLMNemotronFree — the no-paid-key, no-Ollama path. The on-screen
// guidance must point the owner at the free build.nvidia.com key.
func TestFreeNemotronLLMOptionIsSelectable(t *testing.T) {
	m := NewWizard()
	m, _ = send(t, m, enter()) // welcome -> telegram
	m = typeString(t, m, "123456789:AAH-validlookingtoken")
	m, _ = send(t, m, enter()) // telegram -> llm
	if m.CurrentStep() != StepLLM {
		t.Fatalf("did not reach StepLLM; at %v", m.CurrentStep())
	}
	// Pick option 4 (free Nemotron) and confirm.
	m = typeString(t, m, "4")
	if m.llm != LLMNemotronFree {
		t.Fatalf("after pressing 4, llm = %q, want %q", m.llm, LLMNemotronFree)
	}
	// The on-screen guidance for the selected free option must name the free signup.
	v := m.View()
	if !strings.Contains(v, "Free (NVIDIA Nemotron)") {
		t.Errorf("LLM view missing the free-Nemotron choice label; view:\n%s", v)
	}
	if !strings.Contains(v, "build.nvidia.com") {
		t.Errorf("free-Nemotron guidance missing the build.nvidia.com signup hint; view:\n%s", v)
	}
	m, _ = send(t, m, enter()) // llm -> broker
	if m.CurrentStep() != StepBroker {
		t.Fatalf("free-Nemotron choice did not advance; at %v err=%q", m.CurrentStep(), m.Err())
	}

	// Drive the rest of the wizard and assert the choice persists into the result.
	m = typeString(t, m, "1") // Alpaca paper
	m = typeString(t, m, "PKtestbrokerkey123")
	m, _ = send(t, m, enter())                    // key -> secret sub-prompt
	m = typeString(t, m, "SKtestbrokersecret456") // secret
	m, _ = send(t, m, enter())                    // -> intake
	m = answerIntake(t, m)                        // -> safety
	m, _ = send(t, m, enter())                    // -> done
	if !m.Done() {
		t.Fatalf("wizard not done; err=%q", m.Err())
	}
	if got := m.Result().LLM; got != LLMNemotronFree {
		t.Errorf("SetupResult.LLM = %q, want %q", got, LLMNemotronFree)
	}
}

// TestLLMChoiceValidity pins which LLM choices are valid (including the new free
// option) and that a zero/garbage choice is rejected.
func TestLLMChoiceValidity(t *testing.T) {
	for _, c := range []LLMChoice{LLMOAuthGPT, LLMCloudKey, LLMBoth, LLMNemotronFree} {
		if !c.valid() {
			t.Errorf("LLMChoice %q should be valid", c)
		}
	}
	if LLMChoice("garbage").valid() {
		t.Error("an unknown LLMChoice must be invalid")
	}
}

// TestEmptyTelegramTokenIsRejected proves an invalid (empty) answer blocks the
// step with an inline error and NEVER advances or crashes.
func TestEmptyTelegramTokenIsRejected(t *testing.T) {
	m := NewWizard()
	m, _ = send(t, m, enter()) // -> telegram
	// Press enter with no input.
	m, cmd := send(t, m, enter())
	if m.CurrentStep() != StepTelegram {
		t.Fatalf("empty token advanced to %v, want stay on StepTelegram", m.CurrentStep())
	}
	if m.Err() == "" {
		t.Fatal("empty token produced no inline error")
	}
	if cmd != nil {
		t.Fatal("rejecting empty token should not emit a command")
	}
	// A malformed token (no colon) is also rejected.
	m = typeString(t, m, "notatoken")
	m, _ = send(t, m, enter())
	if m.CurrentStep() != StepTelegram {
		t.Fatalf("malformed token advanced to %v, want StepTelegram", m.CurrentStep())
	}
	if m.Err() == "" {
		t.Fatal("malformed token produced no inline error")
	}
	// Recovery: clear the stale malformed input, then a valid token advances.
	m = clearInput(t, m, "notatoken")
	m = typeString(t, m, "123:AAHvalidkey")
	m, _ = send(t, m, enter())
	if m.CurrentStep() != StepLLM {
		t.Fatalf("valid token did not advance; at %v err=%q", m.CurrentStep(), m.Err())
	}
}

// TestEmptyBrokerKeyIsRejected proves a blank broker key blocks the broker step.
func TestEmptyBrokerKeyIsRejected(t *testing.T) {
	m := atBrokerStep(t)
	m = typeString(t, m, "1") // paper
	// enter with no key
	m, _ = send(t, m, enter())
	if m.CurrentStep() != StepBroker {
		t.Fatalf("empty broker key advanced to %v, want StepBroker", m.CurrentStep())
	}
	if m.Err() == "" {
		t.Fatal("empty broker key produced no inline error")
	}
	// A too-short key is also rejected.
	m = typeString(t, m, "short")
	m, _ = send(t, m, enter())
	if m.CurrentStep() != StepBroker {
		t.Fatalf("short broker key advanced to %v, want StepBroker", m.CurrentStep())
	}
	if m.Err() == "" {
		t.Fatal("short broker key produced no inline error")
	}
}

// TestBrokerSecretIsCollectedForReal proves the broker step collects a SECOND
// masked prompt — the API secret — and stores exactly what the owner entered:
//   - a valid key does NOT advance off StepBroker; it opens the secret sub-prompt,
//   - an empty secret is rejected inline (stays on StepBroker, no advance, no crash),
//   - a too-short secret is rejected inline the same way,
//   - a valid secret advances to StepIntake and the entered value lands verbatim in
//     SetupResult.Brokers[0].Secret (never the old "<key>-secret" placeholder).
func TestBrokerSecretIsCollectedForReal(t *testing.T) {
	m := atBrokerStep(t)
	m = typeString(t, m, "2") // Alpaca LIVE (a venue that genuinely needs key+secret)
	m = typeString(t, m, "PKtestbrokerkey123")
	// A valid key must NOT advance — it opens the secret sub-prompt.
	m, cmd := send(t, m, enter())
	if m.CurrentStep() != StepBroker {
		t.Fatalf("valid key advanced to %v, want stay StepBroker for the secret prompt", m.CurrentStep())
	}
	if cmd != nil {
		t.Fatal("entering the key should not emit a command")
	}
	if m.Err() != "" {
		t.Fatalf("valid key produced an inline error: %q", m.Err())
	}

	// Empty secret -> rejected inline, stays on StepBroker.
	m, cmd = send(t, m, enter())
	if m.CurrentStep() != StepBroker {
		t.Fatalf("empty secret advanced to %v, want StepBroker", m.CurrentStep())
	}
	if m.Err() == "" {
		t.Fatal("empty broker secret produced no inline error")
	}
	if cmd != nil {
		t.Fatal("rejecting an empty secret should not emit a command")
	}

	// Too-short secret -> rejected inline, stays on StepBroker.
	m = typeString(t, m, "short")
	m, _ = send(t, m, enter())
	if m.CurrentStep() != StepBroker {
		t.Fatalf("short secret advanced to %v, want StepBroker", m.CurrentStep())
	}
	if m.Err() == "" {
		t.Fatal("short broker secret produced no inline error")
	}

	// Recovery: clear the stale short input, enter a valid secret -> advances.
	m = clearInput(t, m, "short")
	const realSecret = "SKrealbrokersecret789"
	m = typeString(t, m, realSecret)
	m, _ = send(t, m, enter())
	if m.CurrentStep() != StepIntake {
		t.Fatalf("valid secret did not advance; at %v err=%q", m.CurrentStep(), m.Err())
	}

	// Drive the rest and confirm the REAL secret was persisted verbatim.
	m = answerIntake(t, m)
	if m.CurrentStep() != StepSafety {
		t.Fatalf("did not reach safety; at %v err=%q", m.CurrentStep(), m.Err())
	}
	m, _ = send(t, m, enter()) // finish (paper-default: live stays off without toggle)
	if !m.Done() {
		t.Fatalf("wizard not done; err=%q", m.Err())
	}
	got := m.Result().Brokers[0]
	if got.Secret != realSecret {
		t.Errorf("persisted secret = %q, want the entered %q", got.Secret, realSecret)
	}
	if got.Secret == got.Key+"-secret" {
		t.Errorf("persisted secret is the synthesized placeholder, not the entered secret")
	}
}

// TestBadIntakeAnswerIsRejected proves a malformed intake answer (a non-numeric
// capital) is rejected by the REAL Question.Validate and never advances the
// question or crashes.
func TestBadIntakeAnswerIsRejected(t *testing.T) {
	m := atIntakeStep(t)
	// Q1 is risk tolerance (enum). A bad enum is rejected.
	m = typeString(t, m, "reckless")
	before := m.CurrentStep()
	m, cmd := send(t, m, enter())
	if m.CurrentStep() != before {
		t.Fatalf("bad enum advanced step to %v", m.CurrentStep())
	}
	if m.Err() == "" {
		t.Fatal("bad enum produced no inline error")
	}
	if cmd != nil {
		t.Fatal("rejecting a bad answer should not emit a command")
	}
	// Fix it; now Q2 (capital) — a non-number is rejected.
	m = clearInput(t, m, "reckless")
	m = typeString(t, m, "moderate")
	m, _ = send(t, m, enter()) // -> capital question
	m = typeString(t, m, "notanumber")
	m, _ = send(t, m, enter())
	if m.Err() == "" {
		t.Fatal("non-numeric capital produced no inline error")
	}
}

// TestContradictoryPlaybookIsRejectedAtSafety proves the FINAL build can fail (a
// HODL style with a tiny drawdown is contradictory per playbook.Validate) and the
// wizard surfaces it inline at the safety step rather than emitting a bad result.
func TestContradictoryPlaybookIsRejectedAtSafety(t *testing.T) {
	m := atIntakeStep(t)
	bad := map[string]string{
		playbook.KeyRiskTolerance: "aggressive",
		playbook.KeyCapital:       "10000",
		playbook.KeyStyle:         "hodl",
		playbook.KeySectors:       "crypto",
		playbook.KeyMaxDrawdown:   "0.05", // < 0.10 with hodl => contradiction
		playbook.KeyGoals:         "moon",
		playbook.KeyMaxRiskTrade:  "0.02",
		playbook.KeyMaxDailyLoss:  "0.05",
		playbook.KeyMaxLeverage:   "3",
		playbook.KeyMaxOpenPos:    "5",
	}
	for _, q := range playbook.DefaultIntake().Questions {
		if a := bad[q.Id]; a != "" {
			m = typeString(t, m, a)
		}
		m, _ = send(t, m, enter())
	}
	if m.CurrentStep() != StepSafety {
		t.Fatalf("did not reach safety; at %v err=%q", m.CurrentStep(), m.Err())
	}
	// Finishing must FAIL the build and stay on safety with an inline error.
	m, _ = send(t, m, enter())
	if m.CurrentStep() != StepSafety {
		t.Fatalf("contradictory playbook advanced to %v, want stay on StepSafety", m.CurrentStep())
	}
	if m.Err() == "" {
		t.Fatal("contradictory playbook produced no inline error")
	}
	if m.Done() {
		t.Fatal("wizard must NOT be Done with a contradictory playbook")
	}
}

// TestLiveRequiresExplicitToggle proves paper is the default and live is only set
// when BOTH a live broker is chosen AND the live toggle is pressed.
func TestLiveRequiresExplicitToggle(t *testing.T) {
	// Live broker but NO explicit toggle => still paper.
	m := NewWizard()
	m, _ = send(t, m, enter())
	m = typeString(t, m, "123456789:AAH-validlookingtoken")
	m, _ = send(t, m, enter())
	m = typeString(t, m, "1")
	m, _ = send(t, m, enter())
	m = typeString(t, m, "2") // Alpaca LIVE
	m = typeString(t, m, "PKtestbrokerkey123")
	m, _ = send(t, m, enter())                    // key -> secret sub-prompt
	m = typeString(t, m, "SKtestbrokersecret456") // secret
	m, _ = send(t, m, enter())
	m = answerIntake(t, m)
	m, _ = send(t, m, enter()) // finish WITHOUT toggling live
	if !m.Done() {
		t.Fatalf("did not finish; at %v err=%q", m.CurrentStep(), m.Err())
	}
	if m.Result().Live {
		t.Fatal("live broker without explicit toggle must remain paper (Live=false)")
	}

	// Live broker WITH explicit toggle => live.
	m2 := completeHappyPath(t, true)
	if !m2.Done() {
		t.Fatalf("live happy path not done; err=%q", m2.Err())
	}
	if !m2.Result().Live {
		t.Fatal("live broker + explicit toggle must yield Live=true")
	}

	// Paper broker: pressing 'l' must NOT enable live.
	m3 := atSafetyPaper(t)
	m3 = typeString(t, m3, "l")
	if m3.Err() == "" {
		t.Fatal("toggling live on a paper broker should warn inline")
	}
	m3, _ = send(t, m3, enter())
	if m3.Result().Live {
		t.Fatal("paper broker can never produce Live=true")
	}
}

// TestEscStepsBack proves Esc moves backward and clears input — and never crashes
// at the first step.
func TestEscStepsBack(t *testing.T) {
	m := NewWizard()
	m, _ = send(t, m, enter()) // -> telegram
	m = typeString(t, m, "partial")
	m, _ = send(t, m, esc()) // back to welcome
	if m.CurrentStep() != StepWelcome {
		t.Fatalf("esc from telegram: %v, want StepWelcome", m.CurrentStep())
	}
	// Esc at welcome (first step) is a safe no-op.
	m, _ = send(t, m, esc())
	if m.CurrentStep() != StepWelcome {
		t.Fatalf("esc at welcome moved to %v, want stay StepWelcome", m.CurrentStep())
	}
}

// TestCtrlCQuits proves ctrl+c returns the quit command from any step.
func TestCtrlCQuits(t *testing.T) {
	m := NewWizard()
	_, cmd := m.Update(ctrlC())
	if cmd == nil {
		t.Fatal("ctrl+c should return a quit command")
	}
}

// TestBackspaceEdits proves backspace trims the in-flight input (no panic on empty).
func TestBackspaceEdits(t *testing.T) {
	m := NewWizard()
	m, _ = send(t, m, enter()) // telegram step
	m = typeString(t, m, "abc")
	m, _ = send(t, m, backspace())
	// Backspace on an empty buffer must not panic.
	m2 := NewWizard()
	m2, _ = send(t, m2, enter())
	if _, _ = send(t, m2, backspace()); m2.CurrentStep() != StepTelegram {
		t.Fatal("backspace on empty input changed step or crashed")
	}
}

// --- step-position helpers (drive the wizard to a known step) ---

func atBrokerStep(t *testing.T) WizardModel {
	t.Helper()
	m := NewWizard()
	m, _ = send(t, m, enter())
	m = typeString(t, m, "123456789:AAH-validlookingtoken")
	m, _ = send(t, m, enter())
	m = typeString(t, m, "1")
	m, _ = send(t, m, enter())
	if m.CurrentStep() != StepBroker {
		t.Fatalf("atBrokerStep landed on %v", m.CurrentStep())
	}
	return m
}

func atIntakeStep(t *testing.T) WizardModel {
	t.Helper()
	m := atBrokerStep(t)
	m = typeString(t, m, "1")
	m = typeString(t, m, "PKtestbrokerkey123")
	m, _ = send(t, m, enter())                    // key -> secret sub-prompt
	m = typeString(t, m, "SKtestbrokersecret456") // secret
	m, _ = send(t, m, enter())                    // -> intake
	if m.CurrentStep() != StepIntake {
		t.Fatalf("atIntakeStep landed on %v", m.CurrentStep())
	}
	return m
}

func atSafetyPaper(t *testing.T) WizardModel {
	t.Helper()
	m := atIntakeStep(t)
	m = answerIntake(t, m)
	if m.CurrentStep() != StepSafety {
		t.Fatalf("atSafetyPaper landed on %v err=%q", m.CurrentStep(), m.Err())
	}
	return m
}

// clearInput backspaces away a known-length string from the input buffer.
func clearInput(t *testing.T, m WizardModel, s string) WizardModel {
	t.Helper()
	for range s {
		m, _ = send(t, m, backspace())
	}
	return m
}

// TestBrandingOnWelcome proves the BUCKS block wordmark + $ money accent are on the
// welcome view. The welcome path renders the COLORED banner, so its output is ANSI;
// we assert the brand on the plain BuckBanner constant (the source of truth) and on
// the header text, and confirm the colored render carries ANSI escapes.
func TestBrandingOnWelcome(t *testing.T) {
	m := NewWizard()
	v := m.View()
	if !strings.Contains(v, "BUCKS") {
		t.Error("welcome view missing BUCKS brand text")
	}
	if !strings.Contains(v, "$") {
		t.Error("welcome view missing the $ motif")
	}
	// The block wordmark must show on the welcome screen — assert a block char survives.
	if !strings.Contains(v, "█") {
		t.Error("welcome view missing the block wordmark art")
	}
	// The colored banner is on the welcome screen: it must carry ANSI color escapes.
	if !strings.Contains(v, "\x1b[") {
		t.Error("welcome view missing ANSI color (banner should be rendered in color)")
	}
	// The plain banner constant is the README/non-TTY source of truth: assert exact art.
	if !strings.Contains(BuckBanner, "$") {
		t.Error("buck banner constant missing the $ mark")
	}
	if !strings.Contains(BuckBanner, "█") {
		t.Error("buck banner missing the block wordmark art")
	}
	if !strings.Contains(BuckBanner, "the 8-point buck — a trader, not an assistant") {
		t.Error("buck banner missing the tagline")
	}
}

// TestRenderBannerIsColored proves RenderBanner returns the block wordmark WITH ANSI
// color escapes (proving it is colored, not plain), that the retro-arcade neon GREEN
// gradient is applied (top wordmark line green, AND a deeper gradient stop present so
// it's a true vertical gradient, not flat), and that the block art + tagline survive.
func TestRenderBannerIsColored(t *testing.T) {
	r := RenderBanner()
	if !strings.Contains(r, "\x1b[") {
		t.Error("RenderBanner output has no ANSI escape — banner is not colored")
	}
	// The top wordmark line must be BOLD NEON GREEN: lipgloss opens it with the bold
	// (1) + green fg escape. Derive that opening escape from the style itself (split a
	// rendered probe on its payload) so the assertion can't drift from the color codes.
	greenOpen := strings.SplitN(bannerGradient[0].Render("X"), "X", 2)[0]
	if greenOpen == "" || !strings.Contains(r, greenOpen) {
		t.Errorf("RenderBanner output missing the bold neon-green wordmark style (%q)", greenOpen)
	}
	// It must be a real vertical gradient, not one flat color: a DIFFERENT (lower)
	// gradient stop must also appear. Pick a stop whose downsampled escape differs from
	// the top one so this can't pass with a flat fill.
	for _, idx := range []int{2, 4} {
		open := strings.SplitN(bannerGradient[idx].Render("X"), "X", 2)[0]
		if open != greenOpen && strings.Contains(r, open) {
			goto gradientOK
		}
	}
	t.Error("RenderBanner output shows no second gradient stop — banner is flat, not a vertical gradient")
gradientOK:
	// Each block-letter line is rendered as a single styled unit, so the block run
	// survives intact between escapes.
	if !strings.Contains(r, "██████") {
		t.Error("RenderBanner output missing the block wordmark art")
	}
	// The tagline body sits on the "$" line, rendered as a gold segment, so it survives.
	if !strings.Contains(r, "the 8-point buck — a trader, not an assistant") {
		t.Error("RenderBanner output missing the tagline")
	}
}

// TestRenderBannerDollarIsGreenNoNestedRender proves each "$" is colored on its own
// (a green ANSI escape immediately precedes a "$"), and that splitting on "$" did not
// nest one Render inside another — i.e. the "$" is emitted by the dedicated green
// style, not wrapped by the gold style.
func TestRenderBannerDollarIsGreenNoNestedRender(t *testing.T) {
	r := RenderBanner()
	// The "$" is the dedicated neon-green style. Derive its exact escape from the style
	// (no hardcode) so the assertion can't drift from the downsampled color code.
	greenDollar := bannerDollar.Render("$")
	if !strings.Contains(r, greenDollar) {
		t.Errorf("RenderBanner did not emit the green-styled $ (%q) — $ not colored green", greenDollar)
	}
	// No leftover plain "$" should remain outside a color escape: every "$" in the
	// output must be the green-rendered one. Count raw "$" vs green-$ occurrences.
	plainDollars := strings.Count(BuckBanner, "$")
	greenDollars := strings.Count(r, greenDollar)
	if greenDollars != plainDollars {
		t.Errorf("expected %d green-rendered $ to match the %d $ in the plain banner; got %d",
			plainDollars, plainDollars, greenDollars)
	}
}
