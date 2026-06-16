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
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"bucks/internal/tui"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "bucks:", err)
		os.Exit(1)
	}
}

// run parses flags and dispatches. It is split out from main so the dispatch is
// testable without exiting the process.
func run(args []string) error {
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

	// `bucks read <url>` — the direct "read this page and tell me plain-English" path
	// (keyless, no search). Reads the URL read-only and summarizes it, citing the URL.
	if len(args) > 0 && args[0] == "read" {
		url := ""
		if len(args) > 1 {
			url = args[1]
		}
		return runReadStdio(url)
	}

	fs := flag.NewFlagSet("bucks", flag.ContinueOnError)
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
