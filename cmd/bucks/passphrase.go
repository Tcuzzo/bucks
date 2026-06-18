package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/term"

	"bucks/internal/secrets"
	"bucks/internal/tui"
)

// maxPassphraseAttempts bounds how many times the setup prompt re-asks when the two
// entries don't match (or one is empty). It is small and FIXED so a user who keeps
// fumbling gets a clear error instead of an infinite loop — but generous enough that a
// couple of honest typos don't abort setup.
const maxPassphraseAttempts = 4

// passphrasePrompter reads a passphrase from the owner with NO ECHO (the typed
// characters never appear on screen). It is a package var so tests can swap in a stub
// that returns canned answers without a real TTY; production uses readPassphraseNoEcho.
// The prompt string is written to stderr first (stderr so it never pollutes piped
// stdout), then the secret is read from the terminal.
var passphrasePrompter = readPassphraseNoEcho

// stdinIsTerminal reports whether stdin is an interactive terminal. It gates the unlock
// prompt: BUCKS prompts for a passphrase only when a human is attached, NEVER under a
// service manager / piped input (where it would hang or be wrong). It is a package var
// so tests can pin it without a real TTY; production checks the real stdin.
var stdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// readPassphraseNoEcho is the production prompter: it prints the prompt to stderr and
// reads one line from the terminal with echo DISABLED via golang.org/x/term, so the
// passphrase is never shown or left in the scrollback. It returns the typed string with
// the trailing newline already stripped by ReadPassword.
func readPassphraseNoEcho(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr) // ReadPassword swallows the Enter; emit the newline ourselves
	if err != nil {
		return "", fmt.Errorf("read passphrase: %w", err)
	}
	return string(b), nil
}

// promptNewPassphrase asks the owner to choose a passphrase, confirming it by asking
// twice. The two entries must MATCH and be NON-EMPTY; on a mismatch or an empty entry it
// re-prompts up to maxPassphraseAttempts times, then returns a clear error. It NEVER
// returns an unconfirmed or empty passphrase — so a half-typed secret can't be saved.
func promptNewPassphrase() (string, error) {
	for attempt := 0; attempt < maxPassphraseAttempts; attempt++ {
		first, err := passphrasePrompter("Choose a passphrase to protect your keys on this machine: ")
		if err != nil {
			return "", fmt.Errorf("passphrase prompt: %w", err)
		}
		second, err := passphrasePrompter("Confirm passphrase: ")
		if err != nil {
			return "", fmt.Errorf("passphrase prompt: %w", err)
		}
		if first == "" {
			fmt.Fprintln(os.Stderr, "An empty passphrase is no protection — please type one.")
			continue
		}
		if first != second {
			fmt.Fprintln(os.Stderr, "The passphrases didn't match — please try again.")
			continue
		}
		return first, nil
	}
	return "", fmt.Errorf("could not set a passphrase after %d attempts (entries kept mismatching or were empty)", maxPassphraseAttempts)
}

// persistSetupInteractive saves a completed wizard result, handling the keychain-less
// box securely: it first tries persistSetup with the env passphrase (which is empty when
// the owner never set BUCKS_PASSPHRASE). If that fails specifically because a passphrase
// is required (errors.Is secrets.ErrPassphraseRequired) — the no-keychain case — it
// PROMPTS the owner to choose one (enter + confirm, bounded re-prompts) and retries the
// save with it. Any OTHER error is returned as-is (we only prompt for the passphrase
// problem). On the prompted success it prints a plain-English note explaining that to
// run BUCKS non-interactively later (e.g. as a --daemon) the owner sets BUCKS_PASSPHRASE
// to the same passphrase.
//
// secretOpts mirrors persistSetup: empty in production (keychain preferred), and
// secrets.ForceFileBackend() in tests so the round trip is hermetic.
func persistSetupInteractive(r tui.SetupResult, configPath, envPass string, secretOpts ...secrets.Option) error {
	err := persistSetup(r, configPath, envPass, secretOpts...)
	if err == nil {
		return nil
	}
	if !errors.Is(err, secrets.ErrPassphraseRequired) {
		// Not a passphrase problem — surface the real error unchanged.
		return err
	}

	// Keychain-less box, no env passphrase: securely ask the owner to choose one.
	fmt.Fprintln(os.Stderr, "This machine has no system keychain, so BUCKS protects your keys with a passphrase you choose.")
	pass, err := promptNewPassphrase()
	if err != nil {
		return err
	}
	if err := persistSetup(r, configPath, pass, secretOpts...); err != nil {
		return fmt.Errorf("save setup with the chosen passphrase: %w", err)
	}
	fmt.Fprintln(os.Stderr, "Your keys are saved (encrypted with your passphrase).")
	fmt.Fprintln(os.Stderr, "To start BUCKS without typing it each time (e.g. as a --daemon service), set BUCKS_PASSPHRASE to this same passphrase.")
	return nil
}

// promptUnlockPassphrase asks the owner ONCE (no confirm — they're unlocking, not
// choosing) for the passphrase that decrypts the saved keys. Used on the LOAD side of a
// keychain-less box. An empty entry is still rejected (it could never be the right
// passphrase) with a single re-ask bounded by maxPassphraseAttempts.
func promptUnlockPassphrase() (string, error) {
	for attempt := 0; attempt < maxPassphraseAttempts; attempt++ {
		pass, err := passphrasePrompter("Enter your BUCKS passphrase to unlock your keys: ")
		if err != nil {
			return "", fmt.Errorf("passphrase prompt: %w", err)
		}
		if pass != "" {
			return pass, nil
		}
		fmt.Fprintln(os.Stderr, "Please type your passphrase.")
	}
	return "", fmt.Errorf("no passphrase entered after %d attempts", maxPassphraseAttempts)
}
