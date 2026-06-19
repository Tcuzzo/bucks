package main

import (
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"bucks/internal/playbook"
	"bucks/internal/secrets"
	"bucks/internal/tui"
)

// --- minimal keystroke helpers: exactly what a real user types at the wizard ---

func e2eEnter() tea.KeyMsg         { return tea.KeyMsg{Type: tea.KeyEnter} }
func e2eRunes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func e2eSend(t *testing.T, m tui.WizardModel, msg tea.Msg) tui.WizardModel {
	t.Helper()
	next, _ := m.Update(msg)
	wm, ok := next.(tui.WizardModel)
	if !ok {
		t.Fatalf("wizard Update returned %T, want tui.WizardModel", next)
	}
	return wm
}

func e2eType(t *testing.T, m tui.WizardModel, s string) tui.WizardModel {
	t.Helper()
	return e2eSend(t, m, e2eRunes(s))
}

// TestEndToEndWizardPersistsAndRelaunchOpensDashboard is the regression test for the
// customer's HARD BLOCKER (v1.2.1): completing the setup wizard once must PERSIST, so
// the NEXT launch opens the dashboard — never the wizard restarting from step 1.
//
// This is the test the unit slices never had: it drives the REAL wizard with simulated
// keystrokes (unsteered — no hand-built SetupResult), persists through the SAME
// persistSetup that runWizard calls, then exercises the REAL run() entry point and
// asserts the relaunch takes the dashboard path on the owner's saved state. It walks
// the exact chain the customer walked, end to end, on the production code path.
func TestEndToEndWizardPersistsAndRelaunchOpensDashboard(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "bucks.yaml")
	fileBackend := secrets.ForceFileBackend()
	// The hermetic file backend encrypts at rest and requires a passphrase (a desktop
	// would use the OS keychain instead, where no passphrase is needed).
	const pass = "bucks-e2e-passphrase"

	// Phase 0 — a fresh machine: no config yet.
	if configExists(cfg) {
		t.Fatal("precondition: no config should exist on a fresh machine")
	}

	// Phase 1 — a real owner types through the whole guided unwrap.
	m := tui.NewWizard()
	m = e2eSend(t, m, e2eEnter())                        // welcome -> telegram
	m = e2eType(t, m, "123456789:AAH-validlookingtoken") // bot token
	m = e2eSend(t, m, e2eEnter())                        // telegram -> llm
	m = e2eType(t, m, "1")                               // OAuth-GPT backend
	m = e2eSend(t, m, e2eEnter())                        // llm -> broker
	m = e2eType(t, m, "1")                               // Alpaca paper (safe default)
	m = e2eType(t, m, "PKtestbrokerkey123")              // broker key
	m = e2eSend(t, m, e2eEnter())                        // key -> secret sub-prompt
	m = e2eType(t, m, "SKtestbrokersecret456")           // broker secret
	m = e2eSend(t, m, e2eEnter())                        // broker -> intake

	// Intake answers — the same set the wizard's own test uses, driven through the
	// real DefaultIntake question order so the test can't drift from the wizard.
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
	for _, q := range playbook.DefaultIntake().Questions {
		if a := answers[q.Id]; a != "" {
			m = e2eType(t, m, a)
		}
		m = e2eSend(t, m, e2eEnter())
	}
	m = e2eSend(t, m, e2eEnter()) // safety -> done

	if !m.Done() {
		t.Fatal("wizard did not reach Done after the full happy path")
	}

	// Phase 2 — persist EXACTLY as runWizard does on completion.
	if err := persistSetup(m.Result(), cfg, pass, fileBackend); err != nil {
		t.Fatalf("persistSetup failed: %v", err)
	}
	if !configExists(cfg) {
		t.Fatal("THE CUSTOMER BUG: completing the wizard left no config on disk")
	}

	// Phase 3 — relaunch: run() must now pick the DASHBOARD, never re-run the wizard.
	var wizardCalled, dashboardCalled bool
	restore := swapEntryFns(
		func(string) error { wizardCalled = true; return nil },
		func(string) error { dashboardCalled = true; return nil },
	)
	defer restore()
	if err := run([]string{"--config", cfg}); err != nil {
		t.Fatalf("relaunch run() errored: %v", err)
	}
	if wizardCalled {
		t.Error("relaunch RE-RAN THE WIZARD — the customer's restart-from-step-1 bug")
	}
	if !dashboardCalled {
		t.Error("relaunch did not open the dashboard")
	}

	// Phase 4 — the dashboard opens on the owner's real saved state, not an empty stub.
	_, snap, _, err := buildDashboardFromConfig(cfg, pass, fileBackend)
	if err != nil {
		t.Fatalf("buildDashboardFromConfig failed: %v", err)
	}
	if snap.Report.Equity.Cmp(m.Result().Playbook.Capital) != 0 {
		t.Errorf("dashboard equity = %s, want the saved capital %s",
			snap.Report.Equity, m.Result().Playbook.Capital)
	}
}
