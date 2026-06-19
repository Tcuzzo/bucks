package main

import (
	"path/filepath"
	"testing"
)

// TestTrustStoreRoundTrip proves a paired chat id persists + reloads (so a wizard-only owner
// pairs once and never needs an env var again), and that unpaired (no env, no file) reads 0.
func TestTrustStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "bucks.yaml")
	t.Setenv("BUCKS_TELEGRAM_CHAT_ID", "")

	if got := loadTrustedChatID(cfg); got != 0 {
		t.Errorf("no env + no file must be unpaired (0), got %d", got)
	}
	if err := saveTrustedChatID(cfg, 778899); err != nil {
		t.Fatalf("saveTrustedChatID: %v", err)
	}
	if got := loadTrustedChatID(cfg); got != 778899 {
		t.Errorf("persisted paired id not reloaded: got %d, want 778899", got)
	}
}

// TestTrustStoreEnvWins proves an explicit BUCKS_TELEGRAM_CHAT_ID overrides a persisted pairing.
func TestTrustStoreEnvWins(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "bucks.yaml")
	if err := saveTrustedChatID(cfg, 111); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUCKS_TELEGRAM_CHAT_ID", "222")
	if got := loadTrustedChatID(cfg); got != 222 {
		t.Errorf("explicit env should win over the persisted id, got %d", got)
	}
}

// TestTrustStoreRefusesZero proves we never persist chat id 0 (which would read as trust-all).
func TestTrustStoreRefusesZero(t *testing.T) {
	if err := saveTrustedChatID(filepath.Join(t.TempDir(), "bucks.yaml"), 0); err == nil {
		t.Error("saveTrustedChatID must refuse to persist chat id 0")
	}
}
