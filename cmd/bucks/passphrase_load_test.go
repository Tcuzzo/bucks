package main

import (
	"path/filepath"
	"testing"

	"bucks/internal/secrets"
)

// swapTerminalState pins stdinIsTerminal to a fixed value (interactive vs not) and
// returns a restore func — so the load-prompt tests can simulate "running attached to a
// terminal" vs "running under a service / piped" without a real TTY.
func swapTerminalState(t *testing.T, isTerminal bool) func() {
	t.Helper()
	prev := stdinIsTerminal
	stdinIsTerminal = func() bool { return isTerminal }
	return func() { stdinIsTerminal = prev }
}

// TestBuildDashboardInteractivePromptsToUnlockOnKeychainlessLoad is the Part C proof:
// a config saved on a keychain-less box (forced file backend, known passphrase) can be
// OPENED with NO env passphrase by prompting ONCE (no confirm) for the passphrase and
// retrying the load. This is the symmetric counterpart to the save-side prompt — without
// it, a headless owner who didn't export BUCKS_PASSPHRASE could save but never re-open.
func TestBuildDashboardInteractivePromptsToUnlockOnKeychainlessLoad(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "saved-with-this-passphrase"

	want := validSetupResult(t)
	if err := persistSetup(want, configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("persistSetup: %v", err)
	}

	// Running attached to a terminal, with NO env passphrase. The unlock prompt is asked
	// ONCE (no confirm) and returns the right passphrase.
	defer swapTerminalState(t, true)()
	defer swapPrompter(t, pass)() // exactly ONE prompt — swapPrompter fails on a second

	model, snap, err := buildDashboardInteractive(configPath, "", secrets.ForceFileBackend())
	if err != nil {
		t.Fatalf("buildDashboardInteractive must unlock via the prompt, got: %v", err)
	}
	// It loaded the real saved state (paper mode, the saved capital).
	if snap.Health.Live {
		t.Error("loaded snapshot must be PAPER")
	}
	if snap.Report.Equity.Cmp(want.Playbook.Capital) != 0 {
		t.Errorf("loaded equity = %s, want %s", snap.Report.Equity, want.Playbook.Capital)
	}
	if _, ok := model.CurrentSnapshot(); !ok {
		t.Error("dashboard model opened empty")
	}
}

// TestBuildDashboardInteractiveUsesEnvPassphraseNoPrompt proves that when BUCKS_PASSPHRASE
// is set, the load succeeds with NO prompt (the prompter is never called).
func TestBuildDashboardInteractiveUsesEnvPassphraseNoPrompt(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "env-load-passphrase"
	if err := persistSetup(validSetupResult(t), configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("persistSetup: %v", err)
	}

	defer swapTerminalState(t, true)()
	defer swapPrompter(t)() // zero scripted answers: any prompt fails the test

	if _, _, err := buildDashboardInteractive(configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("with the env passphrase, load must succeed with no prompt: %v", err)
	}
}

// TestBuildDashboardInteractiveDoesNotPromptWhenNotATerminal is the daemon / piped-input
// guard (Part C): when stdin is NOT a terminal (e.g. running under a service manager),
// the load MUST NOT prompt — it returns the underlying ErrPassphraseRequired so the
// caller can print the clear "set BUCKS_PASSPHRASE" message. No hang waiting on a TTY
// that isn't there.
func TestBuildDashboardInteractiveDoesNotPromptWhenNotATerminal(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "headless-no-tty-pass"
	if err := persistSetup(validSetupResult(t), configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("persistSetup: %v", err)
	}

	defer swapTerminalState(t, false)() // NOT a terminal — never prompt
	defer swapPrompter(t)()             // any prompt call fails the test

	_, _, err := buildDashboardInteractive(configPath, "", secrets.ForceFileBackend())
	if err == nil {
		t.Fatal("non-interactive load with no passphrase must fail (not silently succeed, not prompt)")
	}
}
