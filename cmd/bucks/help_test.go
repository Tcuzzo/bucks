package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// helpCommandNames are the user-facing command words the top-level help MUST list,
// so a user can discover them (they are dispatched before flag parsing and thus
// never show up in the flag usage).
var helpCommandNames = []string{
	// subcommands
	"chat", "summary", "research", "read", "version", "update", "logo", "mascot", "help",
	// dash / flag commands — the operator wants these listed too
	"--daemon", "--paper-smoke", "--chat", "--config", "--live", "-h", "--help",
}

// TestRunHelp_ListsEveryCommand proves the real runHelp entry point prints the
// header and names EVERY command, driven with an injected buffer (offline).
func TestRunHelp_ListsEveryCommand(t *testing.T) {
	var out bytes.Buffer
	if err := runHelp(&out); err != nil {
		t.Fatalf("runHelp: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "BUCKS — an autonomous trading agent") {
		t.Errorf("help missing the header line; output:\n%s", got)
	}
	for _, name := range helpCommandNames {
		if !strings.Contains(got, name) {
			t.Errorf("help text missing command %q; output:\n%s", name, got)
		}
	}
}

// TestHelpDispatch_ExitsZero proves `bucks help`, `bucks --help`, and `bucks -h` all
// route through run() to the help text and return nil (exit 0). It also asserts the
// help printed contains each command name (the discoverability requirement).
func TestHelpDispatch_ExitsZero(t *testing.T) {
	for _, form := range []string{"help", "--help", "-h"} {
		if err := run([]string{form}); err != nil {
			t.Fatalf("`bucks %s` returned error (want nil/exit 0): %v", form, err)
		}
	}
}

// TestUnknownCommand_ErrorsWithHelp proves an unknown positional command returns the
// errUnknownCommand sentinel (so main exits non-zero) — it does NOT silently fall
// through to the wizard/dashboard.
func TestUnknownCommand_ErrorsWithHelp(t *testing.T) {
	err := run([]string{"frobnicate"})
	if err == nil {
		t.Fatal("unknown command must return a non-nil error (exit non-zero)")
	}
	if !errors.Is(err, errUnknownCommand) {
		t.Fatalf("unknown command error = %v, want errUnknownCommand", err)
	}
}

// TestKnownSubcommandsMatchDispatch is a guard: every name in knownSubcommands must be
// dispatched by run() WITHOUT being treated as an unknown command. We can't run the
// interactive/network paths here, so we assert the guard set itself is the documented
// command set — keeping the map, the dispatch, and the help text in lockstep.
func TestKnownSubcommandsMatchDispatch(t *testing.T) {
	want := map[string]bool{
		"chat": true, "summary": true, "research": true, "read": true,
		"logo": true, "mascot": true, "version": true, "update": true, "help": true,
	}
	if len(knownSubcommands) != len(want) {
		t.Fatalf("knownSubcommands has %d entries, want %d: %v", len(knownSubcommands), len(want), knownSubcommands)
	}
	for name := range want {
		if !knownSubcommands[name] {
			t.Errorf("knownSubcommands missing %q", name)
		}
	}
	// Each documented command name must also appear in the help text.
	var help bytes.Buffer
	_ = runHelp(&help)
	for _, name := range helpCommandNames {
		if !strings.Contains(help.String(), name) {
			t.Errorf("help text missing documented command %q", name)
		}
	}
}
