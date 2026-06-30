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
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"bucks/internal/channel"
	"bucks/internal/orders"
	"bucks/internal/secrets"
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
	"doctor":   true,
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
		return runSummaryStdio(args[1:])
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

	// `bucks doctor` — inspect the installed/source checkout for update and
	// vulnerability drift. `--check` explains the probes without running scans.
	if len(args) > 0 && args[0] == "doctor" {
		return runDoctor(args[1:])
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
	chatFlag := fs.Bool("chat", false, "open the conversational REPL — talk to BUCKS like a person (backend via BUCKS_CHAT_PROVIDER or BUCKS_CHAT_BASEURL/_KEY/_MODEL)")
	live := fs.Bool("live", false, "arm REAL-MONEY live trading this session (default: paper / monitor-only)")
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
		// Headless: no terminal program. Stand up BUCKS's single always-on Telegram
		// gateway so the operator can reach the trader (/status, /halt, /resume, …) with
		// no TTY attached, and shut down gracefully on Ctrl-C / SIGTERM. runDaemonProcess
		// installs the signal-aware context and runs the long-poll loop until a signal.
		fmt.Println("bucks: running headless (daemon) — no TUI attached")
		return runDaemonProcess(*configPath, *live)
	}

	// The single, real entry-point selection: a present config opens the live
	// dashboard; an absent one runs the first-run wizard. Both are dispatched through
	// injectable package vars (runWizardFn / runDashboardFn) so the dispatch LOGIC is
	// testable without launching a real TUI — a test swaps them for spies and asserts
	// which path was taken (see TestRunDispatches*). Production keeps the real TTY funcs.
	if !configExists(*configPath) {
		return runWizardFn(*configPath)
	}
	return runDashboardFn(*configPath)
}

// runWizardFn and runDashboardFn are the two boot paths, indirected behind package
// vars so the dispatch in run() is unit-testable (a test swaps in spies). In
// production they are the real terminal funcs below; nothing else reassigns them.
var (
	runWizardFn    = runWizard
	runDashboardFn = runDashboard
)

// runWizard boots the guided-unpack wizard against the real terminal, and — THIS is
// the fix for the "wizard restarts every launch" blocker — PERSISTS the completed
// result so the config file exists afterward and the next launch opens the dashboard.
// If the owner quits before finishing (no StepDone), nothing is saved and we say so,
// so an aborted setup is never mistaken for a completed one.
func runWizard(configPath string) error {
	p := tea.NewProgram(tui.NewWizard())
	final, err := p.Run()
	if err != nil {
		return err
	}
	model, ok := final.(tui.WizardModel)
	if !ok || !model.Done() {
		// The owner quit before completing setup. Do NOT save a partial result —
		// the wizard will run again next time, which is the correct behavior here.
		fmt.Println("bucks: setup not completed — nothing saved. Run `bucks` to start the wizard again.")
		return nil
	}
	// persistSetupInteractive handles the keychain-less box: if saving needs a passphrase
	// (no system keychain, no BUCKS_PASSPHRASE) it securely PROMPTS for one and retries,
	// so first-run setup completes instead of failing with a cryptic error and re-running
	// the wizard every launch. On a desktop with a keychain it saves with no prompt.
	if err := persistSetupInteractive(model.Result(), configPath, passphraseFromEnv()); err != nil {
		return fmt.Errorf("save setup: %w", err)
	}
	fmt.Println("bucks: setup saved. Run `bucks` to open your dashboard.")
	return nil
}

// runDashboard boots the live dashboard against the real terminal, OPENED on the
// owner's saved setup (mode, account equity from the playbook, flat positions). It
// LoadSetup's the persisted config and seeds the model with an initial snapshot so the
// dashboard shows real loaded state, not an empty stub. A load error (e.g. wrong
// passphrase / missing secrets) is reported in plain English — it does NOT crash and
// does NOT silently fall back to re-running the wizard (that would hide the real
// problem). The live trade-loop feed lands in a later slice; this is the open-on-load.
func runDashboard(configPath string) error {
	// buildDashboardInteractive handles the keychain-less box: with no BUCKS_PASSPHRASE
	// and a human attached, it prompts ONCE to unlock; under a daemon / piped input it
	// does NOT prompt and returns the clear passphrase error handled below.
	model, _, r, err := buildDashboardInteractive(configPath, passphraseFromEnv())
	if err != nil {
		fmt.Fprintf(os.Stderr, "bucks: couldn't open your saved setup: %v\n", err)
		fmt.Fprintln(os.Stderr, "Your config is at:", configPath)
		fmt.Fprintln(os.Stderr, "If you set a passphrase, make sure BUCKS_PASSPHRASE matches it, then run `bucks` again.")
		return err
	}
	// Start the Telegram gateway in the background so the bot the owner configured during
	// setup actually responds on THIS normal launch (not only under --daemon — the gap that
	// left their remote /halt kill switch dead). Tied to the TUI's lifetime: stop() shuts the
	// long-poll loop down cleanly when the dashboard exits. No token -> it's a no-op.
	stop := startLaunchGateway(configPath, r)
	defer stop()
	p := tea.NewProgram(model)
	_, err = p.Run()
	return err
}

// passphraseFromEnv reads the file-backend passphrase from BUCKS_PASSPHRASE. On a
// desktop the OS keychain backend is used and this passphrase is unused (it may be
// empty); on a headless box without a keychain it unlocks the encrypted secrets file.
func passphraseFromEnv() string { return os.Getenv("BUCKS_PASSPHRASE") }

// persistSetup saves a COMPLETED wizard SetupResult so the config file exists on disk
// afterward (configExists -> true) — the durable fix for the wizard re-running every
// launch. It is the thin, testable seam over boot.SaveSetup (playbook plain + secrets
// encrypted). secretOpts is empty in production (keychain preferred) and
// secrets.ForceFileBackend() in tests so the round trip is hermetic.
func persistSetup(r tui.SetupResult, configPath, passphrase string, secretOpts ...secrets.Option) error {
	return SaveSetup(r, configPath, passphrase, secretOpts...)
}

// buildDashboardFromConfig LoadSetup's the persisted config and builds the initial
// dashboard model + snapshot reflecting the SAVED state: paper/live mode, the chosen
// LLM backend, and the account equity from the playbook capital, with no positions yet
// (the honest flat first-open). All the load LOGIC lives here (testable); the caller's
// only untestable part is the tea.NewProgram(...).Run() I/O shell.
func buildDashboardFromConfig(configPath, passphrase string, secretOpts ...secrets.Option) (tui.DashboardModel, tui.Snapshot, tui.SetupResult, error) {
	r, err := LoadSetup(configPath, passphrase, secretOpts...)
	if err != nil {
		return tui.DashboardModel{}, tui.Snapshot{}, tui.SetupResult{}, err
	}
	snap := initialSnapshot(r)
	// Build the chat responder from the SAVED config so `bucks` (launch) opens with
	// chat available straight after setup — no env vars. A nil/errored responder must
	// NEVER block launch: the dashboard still opens read-only with the
	// "configure a backend" hint, so a chat-build hiccup degrades gracefully.
	var resp tui.ChatResponder
	if cr, cerr := newChatResponder(r); cerr == nil {
		resp = cr // nil when config+env yield no backend -> read-only + hint
	}
	model := tui.NewDashboardWithChat(resp)
	updated, _ := model.Update(tui.SnapshotMsg{Snapshot: snap})
	return updated.(tui.DashboardModel), snap, r, nil
}

// buildDashboardInteractive opens the saved dashboard, handling the keychain-less box on
// the LOAD side symmetrically to the save side: it first tries buildDashboardFromConfig
// with the env passphrase (empty when BUCKS_PASSPHRASE is unset). If that fails ONLY
// because a passphrase is required (errors.Is secrets.ErrPassphraseRequired) — the
// no-keychain case — AND a human is attached (stdin is a terminal), it PROMPTS ONCE (no
// confirm) to unlock and retries. Under a service manager / piped input (not a terminal)
// it does NOT prompt: it returns the ErrPassphraseRequired so runDashboard prints the
// clear "set BUCKS_PASSPHRASE" message instead of hanging on a TTY that isn't there. Any
// other load error (wrong passphrase, corrupt file, missing config) is returned as-is.
func buildDashboardInteractive(configPath, envPass string, secretOpts ...secrets.Option) (tui.DashboardModel, tui.Snapshot, tui.SetupResult, error) {
	model, snap, r, err := buildDashboardFromConfig(configPath, envPass, secretOpts...)
	if err == nil {
		return model, snap, r, nil
	}
	// Only the "needs a passphrase, none given, and a human is here" case prompts.
	if !errors.Is(err, secrets.ErrPassphraseRequired) || envPass != "" || !stdinIsTerminal() {
		return tui.DashboardModel{}, tui.Snapshot{}, tui.SetupResult{}, err
	}
	fmt.Fprintln(os.Stderr, "This machine has no system keychain — unlock your saved keys with your passphrase.")
	pass, perr := promptUnlockPassphrase()
	if perr != nil {
		return tui.DashboardModel{}, tui.Snapshot{}, tui.SetupResult{}, perr
	}
	return buildDashboardFromConfig(configPath, pass, secretOpts...)
}

// initialSnapshot builds the first dashboard snapshot from a loaded SetupResult: the
// account equity is the owner's playbook capital, the trader is flat (no positions, no
// realized/unrealized P&L), and the mode reflects the saved paper/live choice. It is a
// pure value build (no clock read, no I/O) so it is fully testable; the live trade loop
// replaces it with real reports once that feed is wired (a later slice).
func initialSnapshot(r tui.SetupResult) tui.Snapshot {
	backend := string(r.LLM)
	return tui.Snapshot{
		Now: time.Time{}, // no wall-clock read; the live loop supplies real times later
		Report: channel.Report{
			Equity:       r.Playbook.Capital,
			RealizedPL:   orders.ZeroDecimal,
			UnrealizedPL: orders.ZeroDecimal,
			Positions:    nil, // flat on first open — honest, not a stub
		},
		Health: tui.Health{
			Halted:  false,
			Backend: backend,
			Live:    r.Live, // paper by default; load never carries live in this slice
		},
	}
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
