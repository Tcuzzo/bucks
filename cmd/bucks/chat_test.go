package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"bucks/internal/analyst"
	"bucks/internal/chat"
)

// scriptBackend is a deterministic mock analyst.Backend for the REPL test: it returns
// a fixed reply per call so the loop is exercised without any network.
type scriptBackend struct {
	name    string
	replies []string
	idx     int
}

func (b *scriptBackend) Name() string { return b.name }

func (b *scriptBackend) Complete(_ context.Context, _ string) (string, error) {
	r := "ok"
	if b.idx < len(b.replies) {
		r = b.replies[b.idx]
	}
	b.idx++
	return r, nil
}

// mockChatterFactory builds a Chatter over the script backend — the injection seam
// that keeps the REPL test offline.
func mockChatterFactory(replies ...string) chatterFactory {
	return func() (*chat.Chatter, error) {
		be := &scriptBackend{name: "mock", replies: replies}
		return chat.NewChatter(chat.NewPersona(""), []analyst.Backend{be})
	}
}

// TestRunChat_DrivesReplLoop proves the `bucks chat` REPL reads lines, calls the
// Chatter, and prints each reply — the real conversational entry point, driven with a
// mock backend (no network).
func TestRunChat_DrivesReplLoop(t *testing.T) {
	in := strings.NewReader("how's it going\nwhat are you thinking\nexit\n")
	var out bytes.Buffer
	if err := runChat(in, &out, mockChatterFactory("flat today, nothing forced", "watching the open, patient")); err != nil {
		t.Fatalf("runChat: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"BUCKS chat",                 // banner
		"flat today, nothing forced", // reply 1
		"watching the open, patient", // reply 2
		"chat closed",                // clean close on exit
	} {
		if !strings.Contains(got, want) {
			t.Errorf("REPL output missing %q; output:\n%s", want, got)
		}
	}
}

// TestRunChat_NoBackendIsClearNotCrash proves the no-config path: with no backend
// (factory returns nil), the REPL prints a clear message naming the env vars and
// returns nil — it never crashes for lack of an LLM.
func TestRunChat_NoBackendIsClearNotCrash(t *testing.T) {
	in := strings.NewReader("hello\n")
	var out bytes.Buffer
	noBackend := func() (*chat.Chatter, error) { return nil, nil }
	if err := runChat(in, &out, noBackend); err != nil {
		t.Fatalf("runChat with no backend should not error, got: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "no LLM backend configured") {
		t.Errorf("no-backend message missing; output:\n%s", got)
	}
	if !strings.Contains(got, envChatBaseURL) {
		t.Errorf("no-backend message should name %s; output:\n%s", envChatBaseURL, got)
	}
}

// TestRunChat_DispatchedFromRun proves the actual CLI dispatch: both `bucks chat`
// (positional) and `bucks --chat` (flag) reach the chat path. Since no chat env is
// set in the test, the path prints the clear no-backend message and exits 0 — proving
// the entry point is wired without needing a live model.
func TestRunChat_DispatchedFromRun(t *testing.T) {
	// Keep the dispatch offline regardless of the host env: no Ollama endpoint and no
	// OpenAI-compatible provider configured, so the path prints the no-backend message.
	t.Setenv(envChatBaseURL, "")
	t.Setenv(envChatProvider, "")
	// Positional subcommand.
	if err := run([]string{"chat"}); err != nil {
		t.Fatalf("`bucks chat` dispatch errored: %v", err)
	}
	// Flag form.
	if err := run([]string{"--chat"}); err != nil {
		t.Fatalf("`bucks --chat` dispatch errored: %v", err)
	}
}

// TestNewChatterFromEnv_BuildsCloudBackend proves the env factory wires a real
// CloudKeyBackend (reused, no new HTTP client) without making any call — construction
// is offline; only Say would hit the network.
func TestNewChatterFromEnv_BuildsCloudBackend(t *testing.T) {
	c, err := newChatterFromEnv("https://example.test", "key", "qwen3.5:cloud", "dry and plain")
	if err != nil {
		t.Fatalf("newChatterFromEnv: %v", err)
	}
	if c == nil {
		t.Fatal("expected a constructed Chatter")
	}
}

// TestEnvChatBackend_NemotronBuildsOpenAICompat proves BUCKS_CHAT_PROVIDER=nemotron
// selects the OpenAI-compatible backend pointed at NVIDIA NIM with the free Nano
// model — needing ONLY the user's own key (no base URL). Construction is offline; we
// assert the concrete type + its name (the profile id). This is the free-brain wiring.
func TestEnvChatBackend_NemotronBuildsOpenAICompat(t *testing.T) {
	t.Setenv(envChatProvider, "nemotron")
	t.Setenv(envChatBaseURL, "") // deliberately blank — the profile supplies the base URL
	t.Setenv(envChatModel, "")   // deliberately blank — the profile supplies the model
	t.Setenv(envChatKey, "nvapi-user-key")

	backend, err := envChatBackend()
	if err != nil {
		t.Fatalf("envChatBackend: %v", err)
	}
	if backend == nil {
		t.Fatal("nemotron provider with a key must build a backend (no base URL needed)")
	}
	oc, ok := backend.(*analyst.OpenAICompatBackend)
	if !ok {
		t.Fatalf("backend type = %T, want *analyst.OpenAICompatBackend", backend)
	}
	// The nemotron alias resolves to the NVIDIA profile, whose name is "nvidia".
	if oc.Name() != analyst.ProviderNvidia {
		t.Errorf("backend Name() = %q, want %q (NVIDIA NIM)", oc.Name(), analyst.ProviderNvidia)
	}
}

// TestEnvChatBackend_NemotronWithoutKeyIsNoBackend proves a named
// OpenAI-compatible provider with no user key does not build an unauthenticated
// cloud backend that fails later. It should fall through to the same clear
// no-backend guidance as an unconfigured chat.
func TestEnvChatBackend_NemotronWithoutKeyIsNoBackend(t *testing.T) {
	for _, provider := range []string{"nemotron", "openai", "nvidia", "groq", "cerebras", "openrouter"} {
		t.Run(provider, func(t *testing.T) {
			t.Setenv(envChatProvider, provider)
			t.Setenv(envChatBaseURL, "")
			t.Setenv(envChatModel, "")
			t.Setenv(envChatKey, "")

			backend, err := envChatBackend()
			if err != nil {
				t.Fatalf("envChatBackend: %v", err)
			}
			if backend != nil {
				t.Fatalf("%s provider without key/base URL must yield no backend, got %T", provider, backend)
			}
		})
	}
}

// TestEnvChatBackend_HostedProviderBaseURLWithoutKeyIsNoBackend proves that
// pasting a known hosted OpenAI-compatible base URL does not bypass the user-key
// requirement. Those providers need a real per-user bearer key; otherwise chat,
// summary, and research should use the clean no-backend guidance path.
func TestEnvChatBackend_HostedProviderBaseURLWithoutKeyIsNoBackend(t *testing.T) {
	for _, tc := range []struct {
		provider string
		baseURL  string
	}{
		{provider: "nemotron", baseURL: "https://integrate.api.nvidia.com/v1"},
		{provider: "nemotron", baseURL: "https://integrate.api.nvidia.com"},
		{provider: "nvidia", baseURL: "https://integrate.api.nvidia.com/v1/"},
		{provider: "groq", baseURL: "https://api.groq.com/openai/v1"},
		{provider: "groq", baseURL: "https://api.groq.com/openai"},
		{provider: "cerebras", baseURL: "https://api.cerebras.ai/v1"},
		{provider: "cerebras", baseURL: "https://api.cerebras.ai"},
		{provider: "openrouter", baseURL: "https://openrouter.ai/api/v1"},
		{provider: "openrouter", baseURL: "https://openrouter.ai/api"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			t.Setenv(envChatProvider, tc.provider)
			t.Setenv(envChatBaseURL, tc.baseURL)
			t.Setenv(envChatModel, "")
			t.Setenv(envChatKey, "")

			backend, err := envChatBackend()
			if err != nil {
				t.Fatalf("envChatBackend: %v", err)
			}
			if backend != nil {
				t.Fatalf("%s hosted base URL without key must yield no backend, got %T", tc.provider, backend)
			}
		})
	}
}

// TestEnvChatBackend_DefaultProviderHostedBaseURLWithoutKeyIsNoBackend proves
// provider-less env config cannot accidentally treat a pasted hosted OpenAI base
// URL as a keyless Ollama endpoint.
func TestEnvChatBackend_DefaultProviderHostedBaseURLWithoutKeyIsNoBackend(t *testing.T) {
	for _, provider := range []string{"", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			t.Setenv(envChatProvider, provider)
			t.Setenv(envChatBaseURL, "https://api.groq.com/openai/v1")
			t.Setenv(envChatModel, "")
			t.Setenv(envChatKey, "")

			backend, err := envChatBackend()
			if err != nil {
				t.Fatalf("envChatBackend: %v", err)
			}
			if backend != nil {
				t.Fatalf("default provider with hosted base URL and no key must yield no backend, got %T", backend)
			}
		})
	}
}

// TestEnvChatBackend_CustomCompatEndpointMayOmitKey preserves the advanced/local
// OpenAI-compatible path: an explicit base URL is enough to intentionally use a
// no-auth endpoint.
func TestEnvChatBackend_CustomCompatEndpointMayOmitKey(t *testing.T) {
	t.Setenv(envChatProvider, "nemotron")
	t.Setenv(envChatBaseURL, "http://localhost:8000/v1")
	t.Setenv(envChatModel, "local-model")
	t.Setenv(envChatKey, "")

	backend, err := envChatBackend()
	if err != nil {
		t.Fatalf("envChatBackend: %v", err)
	}
	if backend == nil {
		t.Fatal("custom OpenAI-compatible base URL should build a backend even without a key")
	}
	if _, ok := backend.(*analyst.OpenAICompatBackend); !ok {
		t.Fatalf("backend type = %T, want *analyst.OpenAICompatBackend", backend)
	}
}

// TestEnvChatBackend_DefaultIsOllamaUnchanged proves the existing default path is
// untouched: with no provider set and a base URL present, the backend is the
// Ollama-style CloudKeyBackend; with no provider AND no base URL, there is no
// backend (the clean no-config case).
func TestEnvChatBackend_DefaultIsOllamaUnchanged(t *testing.T) {
	t.Setenv(envChatProvider, "")
	t.Setenv(envChatBaseURL, "https://ollama.example")
	t.Setenv(envChatKey, "k")
	t.Setenv(envChatModel, "qwen3.5:cloud")
	backend, err := envChatBackend()
	if err != nil {
		t.Fatalf("envChatBackend: %v", err)
	}
	if _, ok := backend.(*analyst.CloudKeyBackend); !ok {
		t.Fatalf("default backend type = %T, want *analyst.CloudKeyBackend", backend)
	}

	t.Setenv(envChatBaseURL, "")
	backend, err = envChatBackend()
	if err != nil {
		t.Fatalf("envChatBackend (no config): %v", err)
	}
	if backend != nil {
		t.Fatalf("no provider + no base URL must yield no backend, got %T", backend)
	}
}

// TestEnvChatBackend_UnknownProviderErrors proves a bogus BUCKS_CHAT_PROVIDER is a
// clear error (not a silent zero backend that would later fail opaquely).
func TestEnvChatBackend_UnknownProviderErrors(t *testing.T) {
	t.Setenv(envChatProvider, "totally-bogus")
	t.Setenv(envChatKey, "k")
	if _, err := envChatBackend(); err == nil {
		t.Fatal("unknown provider should error")
	}
}

func TestEnvChatBackend_UnknownProviderErrorsWithoutKey(t *testing.T) {
	t.Setenv(envChatProvider, "totally-bogus")
	t.Setenv(envChatKey, "")
	if _, err := envChatBackend(); err == nil {
		t.Fatal("unknown provider should error before no-key fallback")
	}
}
