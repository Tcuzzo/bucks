package main

import (
	"path/filepath"
	"testing"

	"bucks/internal/playbook"
	"bucks/internal/secrets"
	"bucks/internal/tui"
)

// liveArmedSetupResult is the SetupResult an owner produces when they configure a LIVE
// Alpaca broker, turn voice on, and explicitly arm live trading. Its live-arm and voice
// preference MUST survive persistence — otherwise the owner's deliberate, real-money "go
// live" choice is silently thrown away on save and they come back up on paper without
// being told (the operator-reported "setup doesn't save for live trading" gap).
func liveArmedSetupResult(t *testing.T) tui.SetupResult {
	t.Helper()
	pb, err := playbook.BuildPlaybook(map[string]string{
		playbook.KeyRiskTolerance: "aggressive",
		playbook.KeyCapital:       "50000",
		playbook.KeyStyle:         "swing",
		playbook.KeyMaxDrawdown:   "0.20",
	})
	if err != nil {
		t.Fatalf("build playbook: %v", err)
	}
	return tui.SetupResult{
		VoiceEnabled:  true,
		TelegramToken: "123456789:AA-live-token-not-a-real-secret-xx",
		LLM:           tui.LLMCloudKey,
		LLMKey:        "CLOUDKEY-abc12345",
		Brokers: []tui.BrokerCreds{{
			Kind:   tui.BrokerAlpacaLive,
			Key:    "LIVEKEY-abc12345",
			Secret: "LIVESECRET-xyz67890",
		}},
		Playbook: pb,
		Live:     true, // explicitly armed live
	}
}

// TestSaveLoadPersistsLiveArmAndVoice proves the owner's deliberate live-arm and their
// voice preference survive the save -> load round trip. Before the fix, SecretConfigFrom
// dropped r.Live and LoadSetup hardcoded Live:false, so an owner who configured a live
// broker and armed live silently came back up on paper after a restart, and the voice
// choice was lost. (Going live still requires a deliberate per-session confirmation in the
// trade loop — persisting the arm only means BUCKS REMEMBERS the intent, never that it
// auto-trades live on boot.)
func TestSaveLoadPersistsLiveArmAndVoice(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "live-arm-pass"

	want := liveArmedSetupResult(t)
	if err := SaveSetup(want, configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("SaveSetup: %v", err)
	}
	got, err := LoadSetup(configPath, pass, secrets.ForceFileBackend())
	if err != nil {
		t.Fatalf("LoadSetup: %v", err)
	}
	if !got.Live {
		t.Error("live-arm was DROPPED on the round trip — owner armed live but loaded back as paper")
	}
	if !got.VoiceEnabled {
		t.Error("voice preference was DROPPED on the round trip")
	}
	// The live broker creds still round-trip verbatim (the arm is useless without them).
	if len(got.Brokers) != 1 || got.Brokers[0].Kind != tui.BrokerAlpacaLive {
		t.Fatalf("live broker did not round-trip: %+v", got.Brokers)
	}
	if got.Brokers[0].Secret != want.Brokers[0].Secret {
		t.Error("live broker secret did not round-trip")
	}
}

// TestPaperSaveStillLoadsPaper pins the safety default: a PAPER setup (Live=false) must
// round-trip to paper. This guards against the live-arm change accidentally defaulting a
// paper owner into live — paper-in must always be paper-out.
func TestPaperSaveStillLoadsPaper(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "paper-pass"

	if err := SaveSetup(validSetupResult(t), configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("SaveSetup: %v", err)
	}
	got, err := LoadSetup(configPath, pass, secrets.ForceFileBackend())
	if err != nil {
		t.Fatalf("LoadSetup: %v", err)
	}
	if got.Live {
		t.Error("paper setup loaded back as LIVE — safety default violated")
	}
}
