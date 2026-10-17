package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bucks/internal/analyst"
	"bucks/internal/understanding"
)

// candidateFile writes a candidate source file and returns its path.
func candidateFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "keystore.go")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	return path
}

// TestEnvUnderstandBackends_SendsGradingBudgetOnTheWire is the CALL-CONTRACT proof
// on the PRODUCTION factory: it configures the real env seam the shipped binary
// reads, builds the backend through envUnderstandBackends (no test constructor),
// drives a real grade, and asserts what actually went out on the wire. The only
// faked thing is the HTTP server at the far end.
//
// This is the concrete guard against the starved-grading bug: the free default brain is a
// REASONING model, and the backend's stock budget is 1024 — enough for the model to
// think and emit nothing.
func TestEnvUnderstandBackends_SendsGradingBudgetOnTheWire(t *testing.T) {
	var gotMaxTokens int
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotMaxTokens = body.MaxTokens
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"dimensions\":{\"spec_adherence\":4,\"architectural_fit\":4,\"type_safety\":4,\"testability\":4,\"security\":4},\"failures\":[],\"recovery_actions\":[],\"confidence\":0.8}"}}]}`))
	}))
	defer srv.Close()

	t.Setenv(envChatProvider, "nemotron")
	t.Setenv(envChatKey, "nvapi-test-key")
	t.Setenv(envChatBaseURL, srv.URL)
	t.Setenv(envChatModel, "")

	backends, err := envUnderstandBackends()
	if err != nil {
		t.Fatalf("envUnderstandBackends: %v", err)
	}
	if len(backends) != 1 {
		t.Fatalf("envUnderstandBackends returned %d backends, want 1", len(backends))
	}

	got, err := understanding.Grade(context.Background(), backends, understanding.Candidate{
		Name:   "keystore.go",
		Intent: "loadKey must reject an empty key.",
		Code:   "func loadKey(env string) string { return os.Getenv(env) }",
	})
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if !got.Passed() {
		t.Errorf("clean grade Passed() = false, want true; verdict=%q", got.Verdict)
	}
	if gotMaxTokens < 2000 {
		t.Errorf("production grading call sent max_tokens=%d, want >= 2000", gotMaxTokens)
	}
	if gotMaxTokens != understanding.MinGradingTokens {
		t.Errorf("production grading call sent max_tokens=%d, want MinGradingTokens=%d",
			gotMaxTokens, understanding.MinGradingTokens)
	}
	if gotAuth != "Bearer nvapi-test-key" {
		t.Errorf("Authorization = %q, want the user's own key", gotAuth)
	}
}

// TestEnvUnderstandBackends_SharesTheChatSelectionSeam proves the understand path
// picks its provider through the SAME env seam as chat/research/summary — so a
// BUCKS_CHAT_PROVIDER choice can never drift between commands.
func TestEnvUnderstandBackends_SharesTheChatSelectionSeam(t *testing.T) {
	t.Setenv(envChatProvider, "")
	t.Setenv(envChatKey, "")
	t.Setenv(envChatBaseURL, "")
	t.Setenv(envChatModel, "")
	backends, err := envUnderstandBackends()
	if err != nil {
		t.Fatalf("envUnderstandBackends: %v", err)
	}
	if len(backends) != 0 {
		t.Errorf("with nothing configured, got %d backends, want 0 (the clean no-backend case)", len(backends))
	}

	// An unknown provider must surface the analyst's LOUD error, not a silent nil.
	t.Setenv(envChatProvider, "definitely-not-a-provider")
	t.Setenv(envChatKey, "k")
	if _, err := envUnderstandBackends(); err == nil {
		t.Error("an unknown provider must return a loud error")
	}
}

// TestRunUnderstand_NoBackendPrintsGuidanceAndExitsZero proves the no-model path is
// advisory and honest: clear setup guidance in BUCKS's existing voice, exit 0 (never
// a crash, never a gate) — and NOTHING in the output that could be read as approval.
func TestRunUnderstand_NoBackendPrintsGuidanceAndExitsZero(t *testing.T) {
	path := candidateFile(t, "func loadKey() string { return \"\" }")
	var out bytes.Buffer
	noBackends := func() ([]analyst.Backend, error) { return nil, nil }

	if err := runUnderstand(&out, noBackends, path, "loadKey must reject an empty key"); err != nil {
		t.Fatalf("runUnderstand with no backend must exit 0, got: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "UNAVAILABLE") {
		t.Errorf("no-backend output must say UNAVAILABLE; got:\n%s", got)
	}
	for _, forbidden := range []string{"APPROVED", "passed"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("no-backend output contains %q — it must be impossible to read as approval; got:\n%s", forbidden, got)
		}
	}
	for _, want := range []string{envChatProvider, envChatKey, "build.nvidia.com"} {
		if !strings.Contains(got, want) {
			t.Errorf("no-backend output missing setup guidance %q; got:\n%s", want, got)
		}
	}
}

// TestRunUnderstand_MissingFileReportsCleanly proves an unreadable candidate is a
// clear, plain-English report — not a panic and not a pass.
func TestRunUnderstand_MissingFileReportsCleanly(t *testing.T) {
	var out bytes.Buffer
	noBackends := func() ([]analyst.Backend, error) { return nil, nil }
	err := runUnderstand(&out, noBackends, filepath.Join(t.TempDir(), "nope.go"), "some intent")
	if err == nil {
		t.Fatal("an unreadable candidate must surface a loud error")
	}
	if strings.Contains(out.String(), "APPROVED") {
		t.Error("an unreadable candidate must never print APPROVED")
	}
}

// TestRunUnderstand_UsageWhenArgsMissing proves the command explains itself rather
// than failing obscurely, matching `bucks research`'s empty-query behavior.
func TestRunUnderstand_UsageWhenArgsMissing(t *testing.T) {
	var out bytes.Buffer
	noBackends := func() ([]analyst.Backend, error) { return nil, nil }
	if err := runUnderstand(&out, noBackends, "", ""); err != nil {
		t.Fatalf("usage path must exit 0, got: %v", err)
	}
	if !strings.Contains(out.String(), "bucks understand") {
		t.Errorf("usage output missing the command form; got:\n%s", out.String())
	}
}

// TestRunUnderstand_PrintsRefutationInPlainEnglish drives the REAL command end to
// end with a refuting transport and proves the owner SEES the refutation: the
// verdict, the named failure, and the recovery action — advisory, exit 0.
func TestRunUnderstand_PrintsRefutationInPlainEnglish(t *testing.T) {
	path := candidateFile(t, "func loadKey(env string) string {\n\treturn os.Getenv(env)\n}")
	reply := `{"dimensions":{"spec_adherence":0,"architectural_fit":2,"type_safety":2,"testability":1,"security":0},` +
		`"failures":[{"dimension":"spec_adherence","detail":"loadKey never rejects an empty value","cite":"loadKey"}],` +
		`"recovery_actions":["return an error when the env value is empty"],"confidence":0.9}`
	backends := func() ([]analyst.Backend, error) {
		return []analyst.Backend{&stubBackend{reply: reply}}, nil
	}
	var out bytes.Buffer
	if err := runUnderstand(&out, backends, path, "loadKey must reject an empty key"); err != nil {
		t.Fatalf("runUnderstand must stay advisory and exit 0, got: %v", err)
	}
	got := out.String()
	for _, want := range []string{"REJECT", "loadKey never rejects an empty value", "return an error when the env value is empty", "25/100"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
}

// stubBackend is the outermost-transport stand-in for the CLI tests.
type stubBackend struct{ reply string }

func (s *stubBackend) Name() string { return "stub" }

func (s *stubBackend) Complete(_ context.Context, _ string) (string, error) { return s.reply, nil }
