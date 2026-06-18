package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bucks/internal/secrets"
)

// stubPrompter returns a sequence of canned answers (in order) so a test can simulate a
// user typing a passphrase at the (mocked) no-echo prompt. After the scripted answers
// run out it fails the test — an unexpected extra prompt is a bug (e.g. an accidental
// retry loop). It returns the same restore func pattern the other seams use.
func swapPrompter(t *testing.T, answers ...string) func() {
	t.Helper()
	prev := passphrasePrompter
	i := 0
	passphrasePrompter = func(prompt string) (string, error) {
		if i >= len(answers) {
			t.Fatalf("passphrasePrompter called more times than scripted (%d answers); extra prompt: %q", len(answers), prompt)
		}
		a := answers[i]
		i++
		return a, nil
	}
	return func() { passphrasePrompter = prev }
}

// TestPersistSetupInteractivePromptsOnKeychainlessSave is the core Part B proof: on a
// keychain-less box (forced file backend) with an EMPTY env passphrase, persisting a
// completed wizard result does NOT silently fail with a cryptic error — instead it
// obtains a passphrase via the INJECTABLE prompter (enter + confirm), persists the
// config (configExists becomes true), and the saved state round-trips back through
// LoadSetup with that same passphrase. This is the keychain-less first-run fix: the
// wizard no longer re-runs every launch because the save couldn't complete.
func TestPersistSetupInteractivePromptsOnKeychainlessSave(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const typed = "owner-typed-passphrase"

	// The user is prompted twice (enter + confirm) and types the SAME passphrase both
	// times. No env passphrase is set — the headless first-run scenario.
	restore := swapPrompter(t, typed, typed)
	defer restore()

	want := validSetupResult(t)

	if configExists(configPath) {
		t.Fatal("precondition: config must not exist yet")
	}

	// envPass is EMPTY -> on the forced file backend this hits ErrPassphraseRequired,
	// which must trigger the prompt + retry (not a hard failure).
	if err := persistSetupInteractive(want, configPath, "", secrets.ForceFileBackend()); err != nil {
		t.Fatalf("persistSetupInteractive must succeed via the prompt, got: %v", err)
	}

	// It actually persisted -> the next launch opens the dashboard, not the wizard.
	if !configExists(configPath) {
		t.Fatal("persistSetupInteractive did not leave a config on disk (wizard would re-run forever)")
	}

	// And the saved secrets round-trip with the passphrase the OWNER typed at the prompt.
	got, err := LoadSetup(configPath, typed, secrets.ForceFileBackend())
	if err != nil {
		t.Fatalf("LoadSetup with the typed passphrase must round-trip: %v", err)
	}
	if len(got.Brokers) != 1 || got.Brokers[0].Key != want.Brokers[0].Key {
		t.Errorf("broker creds did not round-trip through the prompted passphrase: %+v", got.Brokers)
	}
}

// TestPersistSetupInteractiveNoPromptWhenEnvPassphraseSet proves the prompt is NOT used
// when BUCKS_PASSPHRASE is already provided: the env passphrase saves on the first try,
// so the prompter is never called. (swapPrompter with ZERO scripted answers fails the
// test if the prompter is invoked at all.)
func TestPersistSetupInteractiveNoPromptWhenEnvPassphraseSet(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const envPass = "from-the-environment"

	restore := swapPrompter(t) // no scripted answers: any prompt call fails the test
	defer restore()

	if err := persistSetupInteractive(validSetupResult(t), configPath, envPass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("with an env passphrase set, save must succeed with no prompt: %v", err)
	}
	if !configExists(configPath) {
		t.Fatal("did not persist with the env passphrase")
	}
	if _, err := LoadSetup(configPath, envPass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("round-trip with env passphrase: %v", err)
	}
}

// TestPersistSetupInteractiveRePromptsOnMismatchThenSucceeds proves the confirm/mismatch
// handling (Part B §5): if the two entries differ (or one is empty), the flow re-prompts
// rather than saving a passphrase the owner didn't confirm — and once two matching,
// non-empty entries are given, it saves. The scripted sequence is:
//
//	enter "aaa", confirm "bbb"  -> mismatch, re-prompt
//	enter "ccc", confirm ""     -> empty confirm, re-prompt
//	enter "right", confirm "right" -> match -> save
func TestPersistSetupInteractiveRePromptsOnMismatchThenSucceeds(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const good = "right-passphrase"

	restore := swapPrompter(t, "aaa", "bbb", "ccc", "", good, good)
	defer restore()

	if err := persistSetupInteractive(validSetupResult(t), configPath, "", secrets.ForceFileBackend()); err != nil {
		t.Fatalf("after re-prompting past the mismatches, save must succeed: %v", err)
	}
	if _, err := LoadSetup(configPath, good, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("must round-trip with the finally-confirmed passphrase: %v", err)
	}
}

// TestPersistSetupInteractiveGivesUpAfterTooManyMismatches proves the BOUNDED retry: a
// user who never confirms (every pair mismatches) gets a CLEAR error after a small,
// fixed number of attempts — no infinite loop, no panic, and NOTHING is persisted (a
// half-set passphrase must never land on disk).
func TestPersistSetupInteractiveGivesUpAfterTooManyMismatches(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")

	// Always mismatch: enter "x", confirm "y", forever — but the loop is bounded, so it
	// only consumes maxPassphraseAttempts pairs. Provide exactly that many pairs.
	var answers []string
	for i := 0; i < maxPassphraseAttempts; i++ {
		answers = append(answers, "x", "y")
	}
	restore := swapPrompter(t, answers...)
	defer restore()

	err := persistSetupInteractive(validSetupResult(t), configPath, "", secrets.ForceFileBackend())
	if err == nil {
		t.Fatal("a user who never confirms must get a clear error, not a silent success")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "passphrase") {
		t.Errorf("error must mention the passphrase in plain English, got: %v", err)
	}
	// Nothing was persisted — no half-protected secrets on disk.
	if configExists(configPath) {
		t.Error("a failed passphrase setup must not leave a config on disk")
	}
}

// TestPersistSetupInteractivePropagatesNonPassphraseErrors proves we only prompt for the
// SPECIFIC ErrPassphraseRequired case: an unrelated save failure (here, an unwritable
// config path) is returned as-is and the prompter is never invoked.
func TestPersistSetupInteractivePropagatesNonPassphraseErrors(t *testing.T) {
	// A config path whose PARENT is a regular file, so MkdirAll/WriteFile fails — an
	// error that is NOT ErrPassphraseRequired.
	dir := t.TempDir()
	notADir := filepath.Join(dir, "iamafile")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(notADir, "bucks.yaml")

	restore := swapPrompter(t) // any prompt call fails the test
	defer restore()

	err := persistSetupInteractive(validSetupResult(t), configPath, "", secrets.ForceFileBackend())
	if err == nil {
		t.Fatal("an unwritable config path must surface an error")
	}
	if errors.Is(err, secrets.ErrPassphraseRequired) {
		t.Errorf("a write error must not be reported as a passphrase problem: %v", err)
	}
}
