package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"bucks/internal/secrets"
	"bucks/internal/tui"
)

type aiKeyReader func(prompt string) (string, error)
type aiSettingsSaver func(choice tui.LLMChoice, key string) error

// runSettingsFn is injectable so command dispatch is testable without opening a real
// terminal or touching the owner's encrypted configuration.
var runSettingsFn = runSettingsStdio

func runSettingsStdio(args []string) error {
	fs := flag.NewFlagSet("bucks settings", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", defaultConfigPath(), "path to the BUCKS config file")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: bucks settings [--config <path>]")
		fmt.Fprintln(fs.Output(), "Update the encrypted AI backend used by dashboard, chat, summary, and research.")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return fmt.Errorf("settings: unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if !configExists(*configPath) {
		return fmt.Errorf("settings: no saved setup at %s; run `bucks` to complete first-time setup", *configPath)
	}
	current, pass, err := loadSetupWithUnlock(*configPath, passphraseFromEnv())
	if err != nil {
		return fmt.Errorf("settings: load saved setup: %w", err)
	}
	return runAISettingsForSetup(*configPath, pass, current)
}

func runAISettingsForSetup(configPath, passphrase string, current tui.SetupResult) error {
	save := func(choice tui.LLMChoice, key string) error {
		return updateAISettings(configPath, passphrase, choice, key)
	}
	return runAISettings(os.Stdin, os.Stdout, current, passphrasePrompter, save)
}

// runAISettings is the testable console interaction. Provider selection is ordinary
// text, while API keys always pass through readKey, whose production implementation
// disables terminal echo.
func runAISettings(in io.Reader, out io.Writer, current tui.SetupResult, readKey aiKeyReader, save aiSettingsSaver) error {
	fmt.Fprintln(out, "BUCKS AI settings")
	fmt.Fprintf(out, "Current backend: %s\n\n", aiChoiceLabel(current.LLM))
	fmt.Fprintln(out, "  1) ChatGPT login (no API key; requires the codex CLI)")
	fmt.Fprintln(out, "  2) Ollama Cloud API key")
	fmt.Fprintln(out, "  3) ChatGPT primary + Ollama Cloud fallback")
	fmt.Fprintln(out, "  4) Free NVIDIA Nemotron (recommended; no credit card)")
	fmt.Fprintln(out, "  q) Cancel")

	scanner := bufio.NewScanner(in)
	var choice tui.LLMChoice
	for {
		fmt.Fprint(out, "Choose an AI backend [1-4, q]: ")
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("settings: read selection: %w", err)
			}
			fmt.Fprintln(out, "\nNo changes saved.")
			return nil
		}
		switch strings.ToLower(strings.TrimSpace(scanner.Text())) {
		case "1":
			choice = tui.LLMOAuthGPT
		case "2":
			choice = tui.LLMCloudKey
		case "3":
			choice = tui.LLMBoth
		case "4":
			choice = tui.LLMNemotronFree
		case "q", "quit", "cancel":
			fmt.Fprintln(out, "No changes saved.")
			return nil
		default:
			fmt.Fprintln(out, "Please choose 1, 2, 3, 4, or q.")
			continue
		}
		break
	}

	key := ""
	if aiChoiceNeedsKey(choice) {
		prompt := "Paste the API key (input hidden): "
		canKeep := choice == current.LLM && strings.TrimSpace(current.LLMKey) != ""
		if canKeep {
			prompt = "Paste a replacement API key, or press Enter to keep the saved key (input hidden): "
		}
		entered, err := readKey(prompt)
		if err != nil {
			return fmt.Errorf("settings: read API key: %w", err)
		}
		key = strings.TrimSpace(entered)
		if key == "" && canKeep {
			key = current.LLMKey
		}
		if key == "" {
			return errors.New("settings: this backend requires a non-empty API key; no changes saved")
		}
	}

	if err := save(choice, key); err != nil {
		return fmt.Errorf("settings: save AI backend: %w", err)
	}
	fmt.Fprintf(out, "AI backend updated to %s.\n", aiChoiceLabel(choice))
	if choice == tui.LLMOAuthGPT && !codexAvailable() {
		fmt.Fprintln(out, "Note: the codex CLI is not currently on PATH; choose Nemotron if ChatGPT login remains unavailable.")
	}
	return nil
}

// updateAISettings changes only the AI fields in encrypted storage. It deliberately
// does not rewrite bucks.yaml, so playbook state cannot be disturbed by an AI edit.
func updateAISettings(configPath, passphrase string, choice tui.LLMChoice, key string, secretOpts ...secrets.Option) error {
	if !validAIChoice(choice) {
		return fmt.Errorf("unsupported AI backend %q", choice)
	}
	key = strings.TrimSpace(key)
	if aiChoiceNeedsKey(choice) && key == "" {
		return fmt.Errorf("AI backend %q requires an API key", choice)
	}
	store, cfg, err := loadSecretsConfig(configPath, passphrase, secretOpts...)
	if err != nil {
		return fmt.Errorf("load encrypted settings: %w", err)
	}
	cfg.LLMChoice = string(choice)
	if choice == tui.LLMOAuthGPT {
		cfg.LLMKeys = nil
	} else {
		cfg.LLMKeys = []string{key}
	}
	if err := store.Save(cfg); err != nil {
		return fmt.Errorf("save encrypted settings: %w", err)
	}
	return nil
}

// loadSetupWithUnlock shares the existing secure file-backend unlock behavior with
// settings and standalone AI commands. Keyring-backed desktops return immediately.
func loadSetupWithUnlock(configPath, envPass string, secretOpts ...secrets.Option) (tui.SetupResult, string, error) {
	r, err := LoadSetup(configPath, envPass, secretOpts...)
	if err == nil {
		return r, envPass, nil
	}
	if !errors.Is(err, secrets.ErrPassphraseRequired) || envPass != "" || !stdinIsTerminal() {
		return tui.SetupResult{}, "", err
	}
	pass, promptErr := promptUnlockPassphrase()
	if promptErr != nil {
		return tui.SetupResult{}, "", promptErr
	}
	r, err = LoadSetup(configPath, pass, secretOpts...)
	return r, pass, err
}

func validAIChoice(choice tui.LLMChoice) bool {
	switch choice {
	case tui.LLMOAuthGPT, tui.LLMCloudKey, tui.LLMBoth, tui.LLMNemotronFree:
		return true
	default:
		return false
	}
}

func aiChoiceNeedsKey(choice tui.LLMChoice) bool {
	return choice == tui.LLMCloudKey || choice == tui.LLMBoth || choice == tui.LLMNemotronFree
}

func aiChoiceLabel(choice tui.LLMChoice) string {
	switch choice {
	case tui.LLMOAuthGPT:
		return "ChatGPT login"
	case tui.LLMCloudKey:
		return "Ollama Cloud"
	case tui.LLMBoth:
		return "ChatGPT + Ollama Cloud fallback"
	case tui.LLMNemotronFree:
		return "NVIDIA Nemotron"
	default:
		return "not configured"
	}
}
