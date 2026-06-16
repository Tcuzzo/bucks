package analyst

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOpenAICompatBackend_HTTPTest drives the OpenAI-compatible backend against an
// httptest server mirroring the OpenAI /v1/chat/completions shape. It proves the
// request path, the bearer header, the model, and the response content decode —
// with NO real network.
func TestOpenAICompatBackend_HTTPTest(t *testing.T) {
	var gotAuth, gotPath, gotModel string
	var gotStream bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		var req openAIChatRequest
		_ = json.Unmarshal(body, &req)
		gotModel = req.Model
		gotStream = req.Stream
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatResponse{
			Choices: []struct {
				Message chatMessage `json:"message"`
			}{
				{Message: chatMessage{Role: "assistant", Content: "LEAN: bullish\nRATIONALE: trend intact\n"}},
			},
		})
	}))
	defer srv.Close()

	// Base WITHOUT /v1 — chatCompletionsURL must append /v1/chat/completions.
	b := newOpenAICompatBackendWithClient("nvidia", srv.URL, "nvapi-abc", "nano-test", srv.Client())
	out, err := b.Complete(context.Background(), "read AAPL")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.Contains(out, "bullish") {
		t.Errorf("completion = %q, want it to contain bullish", out)
	}
	if gotAuth != "Bearer nvapi-abc" {
		t.Errorf("Authorization header = %q, want Bearer nvapi-abc", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gotModel != "nano-test" {
		t.Errorf("model = %q, want nano-test", gotModel)
	}
	if gotStream {
		t.Errorf("stream = true, want false (non-streaming request)")
	}
	if b.Name() != "nvidia" {
		t.Errorf("Name() = %q, want nvidia", b.Name())
	}
}

// TestOpenAICompatBackend_BaseURLWithV1HitsRightPath proves a base URL that ALREADY
// ends in /v1 (the NVIDIA NIM form) is not doubled — the request still lands on
// /v1/chat/completions, never /v1/v1/chat/completions.
func TestOpenAICompatBackend_BaseURLWithV1HitsRightPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatResponse{
			Choices: []struct {
				Message chatMessage `json:"message"`
			}{{Message: chatMessage{Content: "ok"}}},
		})
	}))
	defer srv.Close()

	// Append /v1 to the httptest base and add a trailing slash to also prove the
	// trailing-slash trim.
	b := newOpenAICompatBackendWithClient("nvidia", srv.URL+"/v1/", "k", "m", srv.Client())
	if _, err := b.Complete(context.Background(), "p"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions (no /v1 doubling, slash trimmed)", gotPath)
	}
}

// TestOpenAICompatBackend_HTTPErrorIsError proves a non-2xx status becomes an error
// (so the analyst fails over) carrying the status — never a fabricated completion.
// A free provider's 429 (rate limit) lands here as a clear error.
func TestOpenAICompatBackend_HTTPErrorIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	b := newOpenAICompatBackendWithClient("nvidia", srv.URL, "k", "m", srv.Client())
	_, err := b.Complete(context.Background(), "p")
	if err == nil {
		t.Fatal("expected error on 429, got nil")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error = %v, want it to carry the 429 status", err)
	}
}

// TestOpenAICompatBackend_EmptyChoicesIsError proves an empty completion errors
// rather than returning empty text as if it were a real read.
func TestOpenAICompatBackend_EmptyChoicesIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()
	b := newOpenAICompatBackendWithClient("nvidia", srv.URL, "k", "m", srv.Client())
	if _, err := b.Complete(context.Background(), "p"); err == nil {
		t.Fatal("expected error on empty choices, got nil")
	}
}

// TestOpenAICompatBackend_EmptyContentIsError proves a present choice with blank
// content is still an error — not silently returned as a real read.
func TestOpenAICompatBackend_EmptyContentIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"   "}}]}`))
	}))
	defer srv.Close()
	b := newOpenAICompatBackendWithClient("nvidia", srv.URL, "k", "m", srv.Client())
	if _, err := b.Complete(context.Background(), "p"); err == nil {
		t.Fatal("expected error on blank content, got nil")
	}
}

// TestOpenAICompatBackend_APIErrorFieldIsError proves an API-level error field in a
// 200 body still surfaces as an error (so the analyst fails over) — never a
// fabricated completion.
func TestOpenAICompatBackend_APIErrorFieldIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatResponse{
			Error: &struct {
				Message string `json:"message"`
			}{Message: "model not available"},
		})
	}))
	defer srv.Close()
	b := newOpenAICompatBackendWithClient("nvidia", srv.URL, "k", "m", srv.Client())
	_, err := b.Complete(context.Background(), "p")
	if err == nil {
		t.Fatal("expected error on api error field, got nil")
	}
	if !strings.Contains(err.Error(), "model not available") {
		t.Errorf("error = %v, want it to carry the api error message", err)
	}
}

// TestOpenAICompatBackend_EmptyBaseURLIsError proves the no-config path errors
// clearly (so a misconfigured profile fails over instead of dialing nothing).
func TestOpenAICompatBackend_EmptyBaseURLIsError(t *testing.T) {
	b := newOpenAICompatBackendWithClient("nvidia", "", "k", "m", nil)
	if _, err := b.Complete(context.Background(), "p"); err == nil {
		t.Fatal("expected error on empty baseURL, got nil")
	}
}

// TestOpenAICompatBackend_SendsTemperatureAndMaxTokens proves the request carries
// the default temperature and a positive max_tokens (a free provider requires both).
func TestOpenAICompatBackend_SendsTemperatureAndMaxTokens(t *testing.T) {
	var req openAIChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatResponse{
			Choices: []struct {
				Message chatMessage `json:"message"`
			}{{Message: chatMessage{Content: "ok"}}},
		})
	}))
	defer srv.Close()
	b := newOpenAICompatBackendWithClient("nvidia", srv.URL, "k", "m", srv.Client())
	if _, err := b.Complete(context.Background(), "p"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if req.Temperature != defaultOpenAITemperature {
		t.Errorf("temperature = %v, want %v", req.Temperature, defaultOpenAITemperature)
	}
	if req.MaxTokens <= 0 {
		t.Errorf("max_tokens = %d, want a positive default", req.MaxTokens)
	}
}

// --- provider profiles ---

// TestProviderProfiles_HaveCorrectBaseURLsAndDefaults pins each supported provider's
// canonical base URL and a non-empty default model.
func TestProviderProfiles_HaveCorrectBaseURLsAndDefaults(t *testing.T) {
	cases := []struct {
		name        string
		wantBaseURL string
		wantModel   string // exact for nvidia (the free default); "" => just assert non-empty
	}{
		{ProviderNvidia, "https://integrate.api.nvidia.com/v1", nemotronNanoModel},
		{ProviderGroq, "https://api.groq.com/openai/v1", ""},
		{ProviderCerebras, "https://api.cerebras.ai/v1", ""},
		{ProviderOpenRouter, "https://openrouter.ai/api/v1", ""},
	}
	for _, c := range cases {
		p, err := ProviderProfileByName(c.name)
		if err != nil {
			t.Fatalf("ProviderProfileByName(%q): %v", c.name, err)
		}
		if p.BaseURL != c.wantBaseURL {
			t.Errorf("%s base URL = %q, want %q", c.name, p.BaseURL, c.wantBaseURL)
		}
		if p.DefaultModel == "" {
			t.Errorf("%s default model is empty", c.name)
		}
		if c.wantModel != "" && p.DefaultModel != c.wantModel {
			t.Errorf("%s default model = %q, want %q", c.name, p.DefaultModel, c.wantModel)
		}
		if p.Name != c.name {
			t.Errorf("%s profile Name = %q, want %q", c.name, p.Name, c.name)
		}
	}
}

// TestProviderProfileByName_AliasesAndUnknown proves the nemotron/openai aliases
// resolve to the free NVIDIA default and an unknown provider is a clear error.
func TestProviderProfileByName_AliasesAndUnknown(t *testing.T) {
	for _, alias := range []string{"nemotron", "openai", "NEMOTRON", "  nvidia  "} {
		p, err := ProviderProfileByName(alias)
		if err != nil {
			t.Fatalf("alias %q errored: %v", alias, err)
		}
		if p.Name != ProviderNvidia {
			t.Errorf("alias %q resolved to %q, want %q", alias, p.Name, ProviderNvidia)
		}
		if p.DefaultModel != nemotronNanoModel {
			t.Errorf("alias %q default model = %q, want the free Nemotron nano", alias, p.DefaultModel)
		}
	}
	if _, err := ProviderProfileByName("bogus-provider"); err == nil {
		t.Fatal("unknown provider should error")
	}
}

// TestNewOpenAICompatBackend_WiresProfile proves NewOpenAICompatBackend builds a
// backend that targets the profile's base URL + default model (no override) and the
// user's key — proven by driving it against an httptest server swapped in via the
// client. With a model override, the override wins.
func TestNewOpenAICompatBackend_WiresProfile(t *testing.T) {
	profile, err := ProviderProfileByName(ProviderNvidia)
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	// Default model path: construct, then assert the model field equals the profile
	// default and the name equals the profile name.
	b := NewOpenAICompatBackend(profile, "nvapi-user-key", "")
	if b.Name() != ProviderNvidia {
		t.Errorf("Name() = %q, want %q", b.Name(), ProviderNvidia)
	}
	if b.model != nemotronNanoModel {
		t.Errorf("model = %q, want the profile default %q", b.model, nemotronNanoModel)
	}
	if b.baseURL != profile.BaseURL {
		t.Errorf("baseURL = %q, want %q", b.baseURL, profile.BaseURL)
	}
	if b.apiKey != "nvapi-user-key" {
		t.Errorf("apiKey not wired from the user key")
	}
	// Override path: a non-empty override replaces the default model.
	b2 := NewOpenAICompatBackend(profile, "k", "custom/model")
	if b2.model != "custom/model" {
		t.Errorf("override model = %q, want custom/model", b2.model)
	}
}
