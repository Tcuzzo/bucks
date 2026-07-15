package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bucks/internal/playbook"
	"bucks/internal/secrets"
	"bucks/internal/tui"
)

// TestConfigChatterNemotronFree proves a SAVED config naming the free NVIDIA
// Nemotron path + a key yields a non-nil Chatter pointed at the NVIDIA backend —
// so chat works right after setup with NO env vars. Asserted via the backend name
// (the provider id), with NO live call.
func TestConfigChatterNemotronFree(t *testing.T) {
	t.Setenv(envChatBaseURL, "")
	t.Setenv(envChatKey, "")
	t.Setenv(envChatProvider, "")

	r := tui.SetupResult{
		LLM:    tui.LLMNemotronFree,
		LLMKey: "nvapi-test-key",
	}

	// The backend seam: a config-sourced Nemotron backend must be the NVIDIA profile.
	backend, err := configChatBackend(r)
	if err != nil {
		t.Fatalf("configChatBackend errored: %v", err)
	}
	if backend == nil {
		t.Fatal("Nemotron-free config with a saved key yielded NO backend")
	}
	if backend.Name() != "nvidia" {
		t.Errorf("backend.Name() = %q, want %q (NVIDIA NIM free brain)", backend.Name(), "nvidia")
	}

	// And the full Chatter must be non-nil (built from the SAVED config, not env).
	ch, err := configChatter(r)
	if err != nil {
		t.Fatalf("configChatter errored: %v", err)
	}
	if ch == nil {
		t.Fatal("configChatter returned nil for a Nemotron-free config with a key")
	}
}

// TestConfigChatterOAuthOnlyNoEnv proves an OAuth-only saved config with NO env
// configured yields (nil, nil) — graceful: no inline-TUI chat backend, never a
// fabricated one. The launch dashboard still opens (read-only + hint).
func TestConfigChatterOAuthOnlyNoEnv(t *testing.T) {
	// Ensure NO env fallback is configured.
	t.Setenv(envChatBaseURL, "")
	t.Setenv(envChatKey, "")
	t.Setenv(envChatProvider, "")
	t.Setenv(envChatModel, "")

	// Simulate a CUSTOMER WITHOUT codex installed: OAuth-GPT then has no codex brain to
	// build, so it must yield nothing (graceful) and the dashboard guides them to the free
	// brain — never a fabricated backend.
	prevCodex := codexAvailable
	codexAvailable = func() bool { return false }
	defer func() { codexAvailable = prevCodex }()

	r := tui.SetupResult{LLM: tui.LLMOAuthGPT}

	backend, err := configChatBackend(r)
	if err != nil {
		t.Fatalf("configChatBackend errored: %v", err)
	}
	if backend != nil {
		t.Errorf("OAuth-GPT with no codex should yield no backend, got %q", backend.Name())
	}

	ch, err := configChatter(r)
	if err != nil {
		t.Fatalf("configChatter errored: %v", err)
	}
	if ch != nil {
		t.Error("OAuth-only config with no env must yield a nil Chatter (graceful no-backend)")
	}
}

// TestConfigChatterFallsBackToEnv proves that when the config yields no backend
// (OAuth-only) BUT the BUCKS_CHAT_* env IS set, configChatter falls back to the env
// backend so the existing env path still works.
func TestConfigChatterFallsBackToEnv(t *testing.T) {
	t.Setenv(envChatProvider, "nemotron")
	t.Setenv(envChatKey, "nvapi-env-key")
	t.Setenv(envChatBaseURL, "")
	t.Setenv(envChatModel, "")

	// No codex -> OAuth-GPT config yields no backend, so the env path must take over.
	prevCodex := codexAvailable
	codexAvailable = func() bool { return false }
	defer func() { codexAvailable = prevCodex }()

	r := tui.SetupResult{LLM: tui.LLMOAuthGPT} // config yields nothing

	ch, err := configChatter(r)
	if err != nil {
		t.Fatalf("configChatter errored: %v", err)
	}
	if ch == nil {
		t.Fatal("configChatter should fall back to the env backend when config yields none")
	}
}

// TestConfigChatterCloudKey proves a saved cloud-key config + a key builds the
// Ollama-style CloudKeyBackend (named "chat-cloud"), so a user who picked the cloud
// key in setup can chat from the dashboard with no env vars.
func TestConfigChatterCloudKey(t *testing.T) {
	t.Setenv(envChatBaseURL, "")
	t.Setenv(envChatKey, "")
	t.Setenv(envChatProvider, "")

	r := tui.SetupResult{
		LLM:    tui.LLMCloudKey,
		LLMKey: "sk-cloud-test",
	}
	backend, err := configChatBackend(r)
	if err != nil {
		t.Fatalf("configChatBackend errored: %v", err)
	}
	if backend == nil {
		t.Fatal("cloud-key config with a saved key yielded NO backend")
	}
	if backend.Name() != "chat-cloud" {
		t.Errorf("backend.Name() = %q, want %q (Ollama-style cloud-key backend)", backend.Name(), "chat-cloud")
	}
}

// TestConfigChatterOAuthUsesCodexWhenAvailable proves the OAuth-GPT choice builds the
// codex-backed ChatGPT brain (no API key) when codex is installed — so a user (operator or a
// customer who installed codex) who picks the default gets a WORKING chat, not a dead end.
func TestConfigChatterOAuthUsesCodexWhenAvailable(t *testing.T) {
	t.Setenv(envChatBaseURL, "")
	t.Setenv(envChatKey, "")
	t.Setenv(envChatProvider, "")
	t.Setenv(envChatModel, "")

	prevCodex := codexAvailable
	codexAvailable = func() bool { return true } // codex installed + logged in
	defer func() { codexAvailable = prevCodex }()

	r := tui.SetupResult{LLM: tui.LLMOAuthGPT}
	backend, err := configChatBackend(r)
	if err != nil {
		t.Fatalf("configChatBackend errored: %v", err)
	}
	if backend == nil {
		t.Fatal("OAuth-GPT with codex available must build the codex brain, got nil")
	}
	if backend.Name() != "oauth-gpt" {
		t.Errorf("backend.Name() = %q, want oauth-gpt (codex brain)", backend.Name())
	}
	ch, err := configChatter(r)
	if err != nil {
		t.Fatalf("configChatter errored: %v", err)
	}
	if ch == nil {
		t.Fatal("OAuth-GPT with codex available must yield a non-nil chatter")
	}
}

// TestConfigChatterBothCodexPrimaryCloudFallback proves "Both" wires codex as the PRIMARY
// brain and the saved cloud key as the ordered FALLBACK — a codex hiccup or quota limit
// fails over to the customer's own key instead of going dark.
func TestConfigChatterBothCodexPrimaryCloudFallback(t *testing.T) {
	t.Setenv(envChatBaseURL, "")
	t.Setenv(envChatKey, "")
	t.Setenv(envChatProvider, "")
	t.Setenv(envChatModel, "")

	prevCodex := codexAvailable
	codexAvailable = func() bool { return true }
	defer func() { codexAvailable = prevCodex }()

	r := tui.SetupResult{LLM: tui.LLMBoth, LLMKey: "sk-cloud-fallback"}
	backends, err := configChatBackends(r)
	if err != nil {
		t.Fatalf("configChatBackends errored: %v", err)
	}
	if len(backends) != 2 {
		t.Fatalf("Both should yield codex primary + cloud fallback (2 backends), got %d", len(backends))
	}
	if backends[0].Name() != "oauth-gpt" || backends[1].Name() != "chat-cloud" {
		t.Errorf("Both order wrong: got %q,%q want oauth-gpt,chat-cloud", backends[0].Name(), backends[1].Name())
	}
}

// TestLLMKeyPersistsRoundTrip proves the saved LLM key survives Save -> Load, so the
// dashboard chat backend can be rebuilt from config with no env vars. Without this,
// config-sourced chat would have no key to authenticate with (the root-cause gap).
func TestLLMKeyPersistsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "llm-key-round-trip"

	pb, err := playbook.BuildPlaybook(map[string]string{
		playbook.KeyRiskTolerance: "moderate",
		playbook.KeyCapital:       "10000",
		playbook.KeyStyle:         "swing",
		playbook.KeyMaxDrawdown:   "0.20",
	})
	if err != nil {
		t.Fatalf("build playbook: %v", err)
	}
	want := tui.SetupResult{
		TelegramToken: "123456789:AA-test-token-not-a-real-secret-xx",
		LLM:           tui.LLMNemotronFree,
		LLMKey:        "nvapi-secret-key-12345", // scan-ok: fixture
		Brokers: []tui.BrokerCreds{{
			Kind:   tui.BrokerAlpacaPaper,
			Key:    "PAPERKEY-abc12345",
			Secret: "PAPERSECRET-xyz67890",
		}},
		Playbook: pb,
	}

	if err := SaveSetup(want, configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("SaveSetup: %v", err)
	}
	got, err := LoadSetup(configPath, pass, secrets.ForceFileBackend())
	if err != nil {
		t.Fatalf("LoadSetup: %v", err)
	}
	if got.LLMKey != want.LLMKey {
		t.Errorf("LLMKey not round-tripped: got %q want %q", got.LLMKey, want.LLMKey)
	}
	if got.LLM != want.LLM {
		t.Errorf("LLM choice not round-tripped: got %q want %q", got.LLM, want.LLM)
	}

	// And the key must NOT appear in the PLAIN config file (encrypted at rest).
	plain, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read plain config: %v", err)
	}
	if strings.Contains(string(plain), "nvapi-secret-key-12345") { // scan-ok: fixture
		t.Error("LLM key leaked into the PLAIN config file — it must be encrypted at rest")
	}
}
