//go:build research_live

// This file is EXCLUDED from the default suite (build tag research_live). It does a
// REAL network fetch + summarize against a live page and a real, env-configured
// model — never run by `go test ./...`. Run it explicitly:
//
//	BUCKS_RESEARCH_URL=https://example.com \
//	BUCKS_CHAT_PROVIDER=nemotron BUCKS_CHAT_KEY=nvapi-... \
//	go test -tags research_live -run TestLive ./internal/research/ -v
//
// With no model env configured it SKIPS (a live-only smoke test, never a default
// gate). It exists to prove the read-only fetch + summarize path works end-to-end on
// real infrastructure, with sources cited.
package research

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"bucks/internal/analyst"
)

// TestLive_FetchAndSummarize does a real read-only fetch of BUCKS_RESEARCH_URL (or a
// safe default) and summarizes it through an env-configured model. It asserts the
// source is cited and the brief is non-empty.
func TestLive_FetchAndSummarize(t *testing.T) {
	url := strings.TrimSpace(os.Getenv("BUCKS_RESEARCH_URL"))
	if url == "" {
		url = "https://example.com"
	}
	backend := liveBackendFromEnv(t)

	c := NewClient(WithUserAgent("bucks-research-live/1.0"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f, err := FetchAndSummarize(ctx, c, []analyst.Backend{backend}, url, "Summarize this page in plain English.")
	if err != nil {
		t.Fatalf("live FetchAndSummarize: %v", err)
	}
	if len(f.Sources) != 1 || f.Sources[0] != url {
		t.Fatalf("expected the source cited, got %v", f.Sources)
	}
	if strings.TrimSpace(f.Brief) == "" {
		t.Fatal("expected a non-empty brief from the live model")
	}
	t.Logf("live brief (%s): %s", f.Backend, f.Brief)
}

// liveBackendFromEnv builds a real analyst.Backend from the BUCKS_CHAT_* env, or
// SKIPS the test when nothing is configured (live-only, never a default gate).
func liveBackendFromEnv(t *testing.T) analyst.Backend {
	t.Helper()
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("BUCKS_CHAT_PROVIDER")))
	key := strings.TrimSpace(os.Getenv("BUCKS_CHAT_KEY"))
	baseURL := strings.TrimSpace(os.Getenv("BUCKS_CHAT_BASEURL"))
	model := strings.TrimSpace(os.Getenv("BUCKS_CHAT_MODEL"))

	switch provider {
	case "", "ollama":
		if baseURL == "" {
			t.Fatal("no live model configured (set BUCKS_CHAT_PROVIDER=nemotron + BUCKS_CHAT_KEY, or BUCKS_CHAT_BASEURL)")
		}
		return analyst.NewCloudKeyBackend("research-live", baseURL, key, model, nil)
	default:
		profile, err := analyst.ProviderProfileByName(provider)
		if err != nil {
			t.Fatalf("unknown provider %q: %v", provider, err)
		}
		if key == "" {
			t.Fatalf("provider %q needs BUCKS_CHAT_KEY", provider)
		}
		if baseURL != "" {
			profile.BaseURL = baseURL
		}
		return analyst.NewOpenAICompatBackend(profile, key, model)
	}
}
