package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bucks/internal/analyst"
	"bucks/internal/research"
)

// researchScriptBackend is a deterministic mock backend for the research/read CLI
// tests: a fixed reply, no network.
type researchScriptBackend struct {
	name  string
	reply string
}

func (b *researchScriptBackend) Name() string { return b.name }

func (b *researchScriptBackend) Complete(_ context.Context, _ string) (string, error) {
	return b.reply, nil
}

// mockResearchBackends is the backendsFactory injection that keeps the CLI test offline.
func mockResearchBackends(reply string) backendsFactory {
	return func() ([]analyst.Backend, error) {
		return []analyst.Backend{&researchScriptBackend{name: "mock", reply: reply}}, nil
	}
}

// mockProvider returns an injected search provider that yields the given URLs (no
// network), satisfying the searchProviderFactory seam.
func mockProvider(urls ...string) searchProviderFactory {
	return func(_ *research.Client) research.SearchProvider {
		var rs []research.Result
		for _, u := range urls {
			rs = append(rs, research.Result{URL: u})
		}
		return research.StaticSearch{Results: rs}
	}
}

// htmlTestServer serves a fixed HTML page (used as a fetch target in CLI tests).
func htmlTestServer(t *testing.T, html string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(html))
	}))
}

// TestRunResearch_DispatchesPrintsBriefAndSources proves `bucks research` searches
// (mock provider), fetches (httptest pages, read-only), summarizes (mock backend),
// and PRINTS the brief AND the cited sources — on the real entry point, offline.
func TestRunResearch_DispatchesPrintsBriefAndSources(t *testing.T) {
	a := htmlTestServer(t, "<title>News A</title><p>Acme beat earnings.</p>")
	defer a.Close()
	b := htmlTestServer(t, "<title>News B</title><p>Analysts raised targets.</p>")
	defer b.Close()

	c := research.NewClient(research.WithHTTPClient(a.Client()))
	var out bytes.Buffer
	err := runResearch(&out, mockResearchBackends("Acme beat earnings; analysts raised targets."),
		mockProvider(a.URL, b.URL), c, "acme earnings")
	if err != nil {
		t.Fatalf("runResearch: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Acme beat earnings; analysts raised targets.") {
		t.Errorf("brief not printed; output:\n%s", got)
	}
	if !strings.Contains(got, "Sources:") {
		t.Errorf("sources header missing; output:\n%s", got)
	}
	// BOTH source URLs must be cited.
	for _, u := range []string{a.URL, b.URL} {
		if !strings.Contains(got, u) {
			t.Errorf("source %q not cited; output:\n%s", u, got)
		}
	}
}

// TestRunResearch_NoBackendIsClearNotCrash proves the no-config path prints a clear
// message naming the env vars and returns nil.
func TestRunResearch_NoBackendIsClearNotCrash(t *testing.T) {
	var out bytes.Buffer
	noBackend := func() ([]analyst.Backend, error) { return nil, nil }
	err := runResearch(&out, noBackend, mockProvider("http://x"), research.NewClient(), "q")
	if err != nil {
		t.Fatalf("runResearch with no backend should not error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "no LLM backend configured") {
		t.Errorf("no-backend message missing; output:\n%s", got)
	}
	if !strings.Contains(got, envChatBaseURL) {
		t.Errorf("no-backend message should name %s; output:\n%s", envChatBaseURL, got)
	}
}

// TestRunResearch_EmptyQueryIsClear proves an empty query prints usage, not a crash.
func TestRunResearch_EmptyQueryIsClear(t *testing.T) {
	var out bytes.Buffer
	if err := runResearch(&out, mockResearchBackends("x"), mockProvider(), research.NewClient(), "   "); err != nil {
		t.Fatalf("empty query should not error: %v", err)
	}
	if !strings.Contains(out.String(), "nothing to research") {
		t.Errorf("expected usage message; output:\n%s", out.String())
	}
}

// TestRunRead_DispatchesPrintsBriefAndSource proves `bucks read <url>` fetches one
// page read-only, summarizes it, and prints the brief + the single cited URL.
func TestRunRead_DispatchesPrintsBriefAndSource(t *testing.T) {
	srv := htmlTestServer(t, "<title>10-K</title><p>Record cash flow reported.</p>")
	defer srv.Close()

	c := research.NewClient(research.WithHTTPClient(srv.Client()))
	var out bytes.Buffer
	if err := runRead(&out, mockResearchBackends("Record cash flow."), c, srv.URL); err != nil {
		t.Fatalf("runRead: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Record cash flow.") {
		t.Errorf("brief not printed; output:\n%s", got)
	}
	if !strings.Contains(got, srv.URL) {
		t.Errorf("source URL not cited; output:\n%s", got)
	}
}

// TestRunRead_DeadURLIsHonest proves a failed fetch prints an honest "no sources read"
// line, never a fabricated brief, and returns nil (no crash).
func TestRunRead_DeadURLIsHonest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := research.NewClient(research.WithHTTPClient(srv.Client()))
	var out bytes.Buffer
	if err := runRead(&out, mockResearchBackends("FABRICATED"), c, srv.URL); err != nil {
		t.Fatalf("runRead should not crash on a dead URL: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "FABRICATED") {
		t.Errorf("dead URL must not print a fabricated brief; output:\n%s", got)
	}
	if !strings.Contains(got, "Sources: none read.") {
		t.Errorf("expected honest no-sources line; output:\n%s", got)
	}
}

// TestRunResearch_DispatchedFromRun proves the actual CLI dispatch reaches the research
// path. With no chat env set, it prints the clear no-backend message and exits 0.
func TestRunResearch_DispatchedFromRun(t *testing.T) {
	t.Setenv(envChatBaseURL, "")
	t.Setenv(envChatProvider, "")
	if err := run([]string{"research", "acme", "earnings"}); err != nil {
		t.Fatalf("`bucks research` dispatch errored: %v", err)
	}
}

// TestRunRead_DispatchedFromRun proves the `bucks read` dispatch is wired. With no
// chat env set, it prints the no-backend message and exits 0.
func TestRunRead_DispatchedFromRun(t *testing.T) {
	t.Setenv(envChatBaseURL, "")
	t.Setenv(envChatProvider, "")
	if err := run([]string{"read", "https://example.com"}); err != nil {
		t.Fatalf("`bucks read` dispatch errored: %v", err)
	}
}

// TestDefaultSearchProvider_IsKeylessDDG proves the production provider factory builds
// the keyless best-effort DuckDuckGo provider over the read-only client (no key
// embedded). Construction is offline.
func TestDefaultSearchProvider_IsKeylessDDG(t *testing.T) {
	sp := defaultSearchProvider(research.NewClient())
	if _, ok := sp.(research.DuckDuckGoSearch); !ok {
		t.Fatalf("default provider type = %T, want research.DuckDuckGoSearch", sp)
	}
}
