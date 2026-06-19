package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDaemonModeSelectsHeadlessPath proves the --daemon flag selects the headless gateway
// path (not the TUI). With a config path that has no saved setup, the daemon returns a
// CLEAR config-load error in plain English — it does NOT attach a TUI (which would fail
// with no terminal) and does NOT crash. (The happy path — a real config, the assembled
// gateway serving commands and halting on the durable kill switch, then graceful shutdown —
// is proven on the runDaemon entry point in daemon_test.go.)
func TestDaemonModeSelectsHeadlessPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-config.yaml")
	err := run([]string{"--daemon", "--config", missing})
	if err == nil {
		t.Fatal("daemon with no saved config should return a clear error, not nil")
	}
	// It took the daemon (load) path, not the TUI: the error is about loading the setup.
	if !strings.Contains(err.Error(), "load setup") && !strings.Contains(err.Error(), "daemon") {
		t.Errorf("expected a clear daemon/load-setup error, got: %v", err)
	}
}

// TestUnknownFlagErrors proves bad flags are reported (ContinueOnError), not
// panicked — run returns the parse error for main to print.
func TestUnknownFlagErrors(t *testing.T) {
	if err := run([]string{"--no-such-flag"}); err == nil {
		t.Fatal("unknown flag should return a parse error")
	}
}

// TestConfigExists proves the config probe distinguishes a present file, a missing
// path, and a directory (which is not a usable config). This is what selects the
// wizard (missing) vs the dashboard (present) at boot.
func TestConfigExists(t *testing.T) {
	dir := t.TempDir()

	if configExists(filepath.Join(dir, "nope.yaml")) {
		t.Error("missing file reported as existing")
	}
	if configExists(dir) {
		t.Error("a directory must not count as a config file")
	}
	if configExists("") {
		t.Error("empty path must not count as existing")
	}

	cfg := filepath.Join(dir, "bucks.yaml")
	if err := os.WriteFile(cfg, []byte("risk_tolerance: moderate\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if !configExists(cfg) {
		t.Error("present config file reported as missing")
	}
}

// TestDefaultConfigPathIsAbsoluteOrFallback proves the default path resolves
// without panicking on any OS — either a real user-config path or the safe
// working-directory fallback. (os.UserConfigDir is the cross-platform resolver.)
func TestDefaultConfigPathIsAbsolute(t *testing.T) {
	p := defaultConfigPath()
	if p == "" {
		t.Fatal("default config path is empty")
	}
	// On a normal box it ends with bucks/bucks.yaml; the fallback is bucks.yaml.
	if filepath.Base(p) != "bucks.yaml" {
		t.Errorf("default config path base = %q, want bucks.yaml", filepath.Base(p))
	}
}
