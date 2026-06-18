// Command bucks is the BUCKS entry point. It is intentionally THIN — it only wires
// the bubbletea models to the real terminal and chooses which one to boot:
//
//   - no config on disk  -> the guided-unpack wizard (first run);
//   - config present      -> the live dashboard;
//   - --daemon            -> headless (no TUI), for running under a service
//     manager where there is no terminal to attach.
//
// All the logic lives in package tui (the testable models) and the other internal
// packages; this file makes none of its own trade/risk/IO decisions. It compiles
// CGO_ENABLED=0 on Linux AND Windows — there is no platform-specific code here.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"bucks/internal/tui"
)

// errUnknownCommand is returned by run() when the first positional arg is not a
// known subcommand. run() has already printed "unknown command: …" + the full help
// to stderr, so main() exits non-zero WITHOUT printing a second, redundant line.
var errUnknownCommand = errors.New("unknown command")

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, errUnknownCommand) {
			// run() already printed the message + help to stderr; just exit non-zero.
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "bucks:", err)
		os.Exit(1)
	}
}

// knownSubcommands is the exact set of positional subcommands run() dispatches
// BEFORE flag parsing. It is the single source of truth used to (a) recognize a
// valid command and (b) reject an unknown one with a helpful message. `mascot` is
// the alias of `logo`. Keep this in lockstep with the dispatch checks below and the
// help text in runHelp.
var knownSubcommands = map[string]bool{
	"chat":     true,
	"summary":  true,
	"research": true,
	"read":     true,
	"logo":     true,
	"mascot":   true,
	"version":  true,
	"update":   true,
	"help":     true,
}

// run parses flags and dispatches. It is split out from main so the dispatch is
// testable without exiting the process.
func run(args []string) error {
	// Top-level help discovery, handled BEFORE flag parsing so the SUBCOMMAND list
	// (not just the flag usage) is what `bucks help`, `bucks -h`, and `bucks --help`
	// print. The positional subcommands below are dispatched before flag.Parse ever
	// runs, so without this check a user could never discover them.
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		return runHelp(os.Stdout)
	}

	// An UNKNOWN positional command (a first arg that is not a flag and not one of the
	// known subcommands) prints a clear error + the help and exits non-zero, instead of
	// silently falling through to the wizard/dashboard. Flags (leading `-`) are left to
	// the flag set below so `--help`-style flags and `-daemon` keep working.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") && !knownSubcommands[args[0]] {
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", args[0])
		_ = runHelp(os.Stderr)
		return errUnknownCommand
	}

	// `bucks chat` — the conversational REPL (you talk to BUCKS like a person). It is
	// a positional subcommand handled BEFORE flag parsing so a bare `bucks chat` works;
	// the `--chat` flag below is the equivalent flag form. The backend is configured by
	// env (BUCKS_CHAT_BASEURL / _KEY / _MODEL); with none it prints a clear message.
	if len(args) > 0 && args[0] == "chat" {
		return runChatStdio()
	}

	// `bucks summary` — print a plain-English, GROUNDED account summary. Like `chat`
	// it is a positional subcommand handled before flag parsing; it reuses the same
	// BUCKS_CHAT_* env backend, and with none it prints a clear message (no crash).
	if len(args) > 0 && args[0] == "summary" {
		return runSummaryStdio()
	}

	// `bucks research "<query>"` — read-only web research: search the web, read the
	// top results, print a plain-English brief with its CITED sources. Like chat, it
	// uses the BUCKS_CHAT_* env backend; with none it prints a clear message (no crash).
	if len(args) > 0 && args[0] == "research" {
		return runResearchStdio(strings.TrimSpace(strings.Join(args[1:], " ")))
	}

	// `bucks logo` (alias `bucks mascot`) — print the colored buck mascot to stdout and
	// exit 0, so the owner can preview the terminal art without launching the wizard. A
	// positional subcommand handled before flag parsing; it needs no config and no LLM.
	if len(args) > 0 && (args[0] == "logo" || args[0] == "mascot") {
		return runLogoStdio()
	}

	// `bucks read <url>` — the direct "read this page and tell me plain-English" path
	// (keyless, no search). Reads the URL read-only and summarizes it, citing the URL.
	if len(args) > 0 && args[0] == "read" {
		url := ""
		if len(args) > 1 {
			url = args[1]
		}
		return runReadStdio(url)
	}

	// `bucks version` — print the build-stamped version + GOOS/GOARCH + go runtime
	// version. A positional subcommand handled before flag parsing; needs no config
	// and makes no network call.
	if len(args) > 0 && args[0] == "version" {
		return runVersionStdio()
	}

	// `bucks update` — the safe self-updater: check the latest GitHub Release, and on
	// confirmation (or --yes) download + CHECKSUM-VERIFY + atomically replace this
	// binary. `bucks update --check` only reports availability. A positional
	// subcommand handled before flag parsing so its own flags don't collide with the
	// top-level set.
	if len(args) > 0 && args[0] == "update" {
		return runUpdateStdio(args[1:])
	}

	fs := flag.NewFlagSet("bucks", flag.ContinueOnError)
	// Usage prints the FULL command list (not just the flag dump), so any path that
	// triggers flag usage still shows how to discover the subcommands.
	fs.Usage = func() { _ = runHelp(os.Stderr) }
	daemon := fs.Bool("daemon", false, "run headless (no TUI) under a service manager")
	paperSmoke := fs.Bool("paper-smoke", false, "boot the saved config into a paper trader and place one in-band paper trade (offline acceptance), then exit")
	chatFlag := fs.Bool("chat", false, "open the conversational REPL — talk to BUCKS like a person (backend via BUCKS_CHAT_BASEURL/_KEY/_MODEL)")
	configPath := fs.String("config", defaultConfigPath(), "path to the BUCKS config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *chatFlag {
		return runChatStdio()
	}

	if *paperSmoke {
		// The real unwrap->config->trader acceptance on the ACTUAL entry point: load
		// the persisted playbook (plain) + secrets (encrypted at rest), wire a paper
		// trader, drive ONE in-band paper decision, and report reaching trading(paper).
		// No live network — this is the offline paper acceptance the shipped zip runs.
		return runPaperSmoke(*configPath)
	}

	if *daemon {
		// Headless: no terminal program. The real loop wiring lands in a later
		// slice; here we only prove the entry point selects the headless path and
		// does not try to attach a TUI (which would fail without a terminal).
		fmt.Println("bucks: running headless (daemon) — no TUI attached")
		return nil
	}

	if !configExists(*configPath) {
		return runWizard()
	}
	return runDashboard()
}

// runWizard boots the guided-unpack wizard against the real terminal.
func runWizard() error {
	p := tea.NewProgram(tui.NewWizard())
	_, err := p.Run()
	return err
}

// runDashboard boots the live dashboard against the real terminal. The harness
// (wired in a later slice) feeds it tui.SnapshotMsg values via p.Send.
func runDashboard() error {
	p := tea.NewProgram(tui.NewDashboard())
	_, err := p.Run()
	return err
}

// configExists reports whether a readable config file is present at path.
func configExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// defaultConfigPath is the per-user config location. It uses os.UserConfigDir so
// the SAME code resolves the right place on Linux (~/.config) and Windows
// (%AppData%) — the cross-platform requirement, with no build tags.
func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		// Fall back to the working directory; never panic on a headless box.
		return "bucks.yaml"
	}
	return filepath.Join(dir, "bucks", "bucks.yaml")
}
