package main

import (
	"path/filepath"
	"testing"

	"bucks/internal/playbook"
	"bucks/internal/secrets"
	"bucks/internal/tui"
)

// liveArmedSetupResult models a legacy saved configuration. The current wizard
// cannot produce it, but loading old encrypted files must remain compatible.
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
		Live:     true, // legacy persisted value; cannot enable real-money trading
	}
}

// TestSaveLoadPersistsLiveArmAndVoice proves legacy data and the voice preference
// survive a save/load round trip. Preserving Live does not enable real-money trading.
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
		t.Error("legacy Live value was dropped on the round trip")
	}
	if !got.VoiceEnabled {
		t.Error("voice preference was DROPPED on the round trip")
	}
	// Legacy broker credentials still round-trip verbatim for data compatibility.
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
