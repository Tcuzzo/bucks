package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"bucks/internal/playbook"
	"bucks/internal/secrets"
	"bucks/internal/tui"
)

// validSetupResult builds a realistic, validated SetupResult exactly as the guided
// unwrap wizard would emit one: a real playbook (built + validated via BuildPlaybook),
// a paper broker, a Telegram token, and PAPER (Live=false). This is the unwrap output
// the boot consumes.
func validSetupResult(t *testing.T) tui.SetupResult {
	t.Helper()
	pb, err := playbook.BuildPlaybook(map[string]string{
		playbook.KeyRiskTolerance: "moderate",
		playbook.KeyCapital:       "25000",
		playbook.KeyStyle:         "swing",
		playbook.KeyMaxDrawdown:   "0.20",
	})
	if err != nil {
		t.Fatalf("build playbook: %v", err)
	}
	return tui.SetupResult{
		VoiceEnabled:  false,
		TelegramToken: "123456789:AA-test-token-not-a-real-secret-xx",
		LLM:           tui.LLMOAuthGPT,
		Brokers: []tui.BrokerCreds{{
			Kind:   tui.BrokerAlpacaPaper,
			Key:    "PAPERKEY-abc12345",
			Secret: "PAPERSECRET-xyz67890",
		}},
		Playbook: pb,
		Live:     false, // paper default
	}
}

// TestPaperTradeBootReachesTrading is THE paper-trade boot acceptance: a loaded
// SetupResult is wired into a real harness.Trader (paper/mock broker, risk engine built
// FROM THE PLAYBOOK, kill switch, mock channel), then ONE in-band paper decision is
// driven through and reaches "trading (paper)" — a paper order actually placed on the
// broker. This proves the unwrap -> config -> trader wiring end-to-end with NO live
// network. Live Alpaca-paper is the operator-gated next step (needs their keys).
func TestPaperTradeBootReachesTrading(t *testing.T) {
	r := validSetupResult(t)
	ksPath := filepath.Join(t.TempDir(), "killswitch.json")

	res, err := bootPaperTrader(context.Background(), r, ksPath)
	if err != nil {
		t.Fatalf("paper-trade boot failed: %v", err)
	}
	if !res.ReachedPaper {
		t.Fatalf("boot did not reach trading (paper); status=%q", res.Status)
	}
	if res.Status != "trading (paper)" {
		t.Errorf("status = %q, want %q", res.Status, "trading (paper)")
	}
	if res.PlacedClOrdID == "" {
		t.Error("a paper order should have been placed, but no client-order-id was recorded")
	}

	// Paper default: the trader must NOT be live.
	if res.Trader.LiveEnabled() {
		t.Error("boot acceptance must run PAPER (LiveEnabled=false)")
	}

	// The mock (paper) broker actually received exactly one order with that id.
	placed := res.Broker.Placed()
	if len(placed) != 1 {
		t.Fatalf("expected exactly 1 paper order placed, got %d: %v", len(placed), placed)
	}
	if placed[0] != res.PlacedClOrdID {
		t.Errorf("placed order id %q != recorded id %q", placed[0], res.PlacedClOrdID)
	}

	// And the trade is in the ledger as auto-placed (within-band paper trade).
	ledger := res.Trader.Ledger()
	if len(ledger) != 1 {
		t.Fatalf("ledger should have 1 record, got %d", len(ledger))
	}
	if !ledger[0].Outcome.Placed() {
		t.Errorf("ledger outcome = %s, want a placed (auto) paper trade", ledger[0].Outcome)
	}
}

// TestSecretConfigFromMapsSensitiveFields proves the TUI->secrets mapping carries the
// sensitive material (broker creds, telegram token, llm choice) into the persisted
// secrets shape — the exact fields that get encrypted at rest.
func TestSecretConfigFromMapsSensitiveFields(t *testing.T) {
	r := validSetupResult(t)
	cfg := SecretConfigFrom(r)
	if cfg.TelegramToken != r.TelegramToken {
		t.Errorf("telegram token not mapped: %q", cfg.TelegramToken)
	}
	if cfg.LLMChoice != string(r.LLM) {
		t.Errorf("llm choice not mapped: %q", cfg.LLMChoice)
	}
	if len(cfg.Brokers) != 1 {
		t.Fatalf("brokers not mapped: %+v", cfg.Brokers)
	}
	if cfg.Brokers[0].Key != "PAPERKEY-abc12345" || cfg.Brokers[0].Secret != "PAPERSECRET-xyz67890" {
		t.Errorf("broker creds not mapped verbatim: %+v", cfg.Brokers[0])
	}
	if cfg.Brokers[0].Kind != string(tui.BrokerAlpacaPaper) {
		t.Errorf("broker kind not mapped: %q", cfg.Brokers[0].Kind)
	}
}

// TestBootDeniedTradeDoesNotReachTrading is a guard-rail proof: if the playbook capital
// is so small that the acceptance trade is no longer within the (capital-scaled) band,
// the trade must NOT silently "succeed" as live — the band floor still admits the tiny
// $100-notional acceptance trade, so this asserts the honest invariant that a placed
// outcome is required to claim "trading (paper)" (no fake-green).
func TestBootStatusHonest(t *testing.T) {
	r := validSetupResult(t)
	ksPath := filepath.Join(t.TempDir(), "ks.json")
	res, err := bootPaperTrader(context.Background(), r, ksPath)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	// Status only reads "trading (paper)" when a trade actually placed.
	if res.ReachedPaper && res.Status != "trading (paper)" {
		t.Errorf("reached-paper but status not trading(paper): %q", res.Status)
	}
	if !res.ReachedPaper && strings.Contains(res.Status, "trading") {
		t.Errorf("did NOT reach paper but status claims trading: %q", res.Status)
	}
}

// TestSaveLoadSetupRoundTripBootsPaper exercises the FULL persistence + boot chain: a
// SetupResult is SAVED (playbook plain + secrets encrypted via the file backend), then
// LOADED back, then booted into a paper trader that reaches trading(paper). It also
// proves the persistence split — the plain config file does NOT contain the broker
// secret (that lives only in the encrypted secrets file).
func TestSaveLoadSetupRoundTripBootsPaper(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "round-trip-passphrase"

	// HERMETIC: force the age FILE backend rooted at this temp dir. Without this the
	// chooser would pick the developer's REAL OS keychain under the global fixed key
	// "bucks-trader/config" — a shared global that this test and the internal/secrets
	// tests clobber when the suite runs concurrently, making the round trip non-
	// deterministic. ForceFileBackend keeps every secret on the temp file (and exercises
	// the persistence-split assertion below, which expects the encrypted secrets.age).
	want := validSetupResult(t)
	if err := SaveSetup(want, configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("SaveSetup: %v", err)
	}

	// The PLAIN config must NOT carry the broker secret — secrets are encrypted apart.
	plain, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read plain config: %v", err)
	}
	if strings.Contains(string(plain), "PAPERSECRET-xyz67890") {
		t.Error("broker SECRET leaked into the PLAIN config file")
	}
	if strings.Contains(string(plain), want.TelegramToken) {
		t.Error("telegram token leaked into the PLAIN config file")
	}

	// The encrypted secrets file (file backend) must NOT contain the secret in
	// plaintext. On a box with an OS keychain the chooser uses the keychain instead and
	// no file is written — in that case the keychain is the at-rest encryption and there
	// is nothing on disk to leak. We assert the file-backend case when the file exists.
	if secBlob, err := os.ReadFile(filepath.Join(dir, "secrets.age")); err == nil {
		if strings.Contains(string(secBlob), "PAPERSECRET-xyz67890") {
			t.Error("broker SECRET found in plaintext inside the encrypted secrets file")
		}
		if !strings.HasPrefix(string(secBlob), "age-encryption.org/v1") {
			t.Error("secrets file is not an age ciphertext")
		}
	}

	// Load it back (same hermetic file backend) and boot a paper trader.
	got, err := LoadSetup(configPath, pass, secrets.ForceFileBackend())
	if err != nil {
		t.Fatalf("LoadSetup: %v", err)
	}
	if len(got.Brokers) != 1 || got.Brokers[0].Secret != want.Brokers[0].Secret {
		t.Errorf("broker creds did not survive the round trip: %+v", got.Brokers)
	}
	res, err := bootPaperTrader(context.Background(), got, filepath.Join(dir, "ks.json"))
	if err != nil {
		t.Fatalf("boot from loaded setup: %v", err)
	}
	if !res.ReachedPaper || res.Status != "trading (paper)" {
		t.Errorf("loaded setup did not reach trading (paper): %q", res.Status)
	}
}

// TestPaperSmokeCLIReachesTradingUnsteered drives the REAL entry point — run() with the
// actual `--paper-smoke` flag — after a save, with NO injection of the trader's
// decisions. This is the "agents must pass on the real entrypoint" proof for the shipped
// zip's offline acceptance: unwrap -> saved config -> `bucks --paper-smoke` -> trading
// (paper). The passphrase is supplied via BUCKS_PASSPHRASE exactly as the runtime reads
// it.
func TestPaperSmokeCLIReachesTradingUnsteered(t *testing.T) {
	// HERMETIC: this test drives the REAL run() entrypoint, whose internal SaveSetup/
	// LoadSetup use the PRODUCTION chooser (keychain preferred) with no test options — we
	// must NOT change that production path. keyring.MockInit swaps go-keyring for an
	// in-process, process-local store, so the real keychain code path is exercised end to
	// end while the developer's actual OS Secret Service is never touched and no other test
	// binary can collide. (MockInit is process-local — separate `go test` binaries each get
	// their own in-memory keychain.)
	keyring.MockInit()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "cli-smoke-pass"

	if err := SaveSetup(validSetupResult(t), configPath, pass); err != nil {
		t.Fatalf("SaveSetup: %v", err)
	}
	t.Setenv("BUCKS_PASSPHRASE", pass)

	// Drive the ACTUAL CLI dispatch — no injected decisions, the real flag path.
	if err := run([]string{"--paper-smoke", "--config", configPath}); err != nil {
		t.Fatalf("`bucks --paper-smoke` failed: %v", err)
	}
}
