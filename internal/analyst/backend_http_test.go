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

// TestOAuTHGPTBackend_HTTPTest drives the OAuth-GPT backend against an httptest
// server mirroring the OpenAI chat-completions shape. It proves the request path,
// the bearer header, and the response decode — with NO real network.
func TestOAuTHGPTBackend_HTTPTest(t *testing.T) {
	var gotAuth, gotPath string
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		var cr chatRequest
		_ = json.Unmarshal(body, &cr)
		gotModel = cr.Model
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatResponse{
			Choices: []struct {
				Message chatMessage `json:"message"`
			}{
				{Message: chatMessage{Role: "assistant", Content: "LEAN: bullish\nRATIONALE: ok\n"}},
			},
		})
	}))
	defer srv.Close()

	b := NewOAuTHGPTBackend("oauth-gpt", srv.URL, "tok-123", "gpt-test", srv.Client())
	out, err := b.Complete(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.Contains(out, "bullish") {
		t.Errorf("completion = %q, want it to contain bullish", out)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization header = %q, want Bearer tok-123", gotAuth)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotModel != "gpt-test" {
		t.Errorf("model = %q, want gpt-test", gotModel)
	}
}

// TestOAuTHGPTBackend_HTTPErrorIsError proves a non-2xx status becomes an error
// (so the analyst fails over) — never a fabricated completion.
func TestOAuTHGPTBackend_HTTPErrorIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	b := NewOAuTHGPTBackend("oauth-gpt", srv.URL, "tok", "m", srv.Client())
	_, err := b.Complete(context.Background(), "p")
	if err == nil {
		t.Fatal("expected error on 429, got nil")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error = %v, want it to carry the 429 status", err)
	}
}

// TestOAuTHGPTBackend_EmptyChoicesIsError proves an empty completion errors
// rather than returning empty text as if it were a real read.
func TestOAuTHGPTBackend_EmptyChoicesIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()
	b := NewOAuTHGPTBackend("oauth-gpt", srv.URL, "tok", "m", srv.Client())
	if _, err := b.Complete(context.Background(), "p"); err == nil {
		t.Fatal("expected error on empty choices, got nil")
	}
}

// TestOAuTHGPTBackend_APIErrorFieldIsError proves an API-level error field in a
// 200 body still surfaces as an error (so the analyst fails over) — never a
// fabricated completion. This mirrors TestCloudKeyBackend_APIErrorFieldIsError for
// the chat-completions shape.
func TestOAuTHGPTBackend_APIErrorFieldIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// HTTP 200 but the body carries an API-level error object.
		_ = json.NewEncoder(w).Encode(chatResponse{
			Error: &struct {
				Message string `json:"message"`
			}{Message: "context length exceeded"},
		})
	}))
	defer srv.Close()
	b := NewOAuTHGPTBackend("oauth-gpt", srv.URL, "tok", "m", srv.Client())
	_, err := b.Complete(context.Background(), "p")
	if err == nil {
		t.Fatal("expected error on api error field, got nil")
	}
	if !strings.Contains(err.Error(), "context length exceeded") {
		t.Errorf("error = %v, want it to carry the api error message", err)
	}
}

// TestCloudKeyBackend_HTTPTest drives the cloud-key backend against an httptest
// server mirroring the Ollama-style generate shape.
func TestCloudKeyBackend_HTTPTest(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(generateResponse{Response: "LEAN: bearish\nRATIONALE: weak\n"})
	}))
	defer srv.Close()

	b := NewCloudKeyBackend("cloud-key", srv.URL, "key-abc", "qwen-test", srv.Client())
	out, err := b.Complete(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.Contains(out, "bearish") {
		t.Errorf("completion = %q, want it to contain bearish", out)
	}
	if gotAuth != "Bearer key-abc" {
		t.Errorf("Authorization header = %q, want Bearer key-abc", gotAuth)
	}
	if gotPath != "/api/generate" {
		t.Errorf("path = %q, want /api/generate", gotPath)
	}
}

// TestCloudKeyBackend_APIErrorFieldIsError proves an API-level error field in a
// 200 body still surfaces as an error.
func TestCloudKeyBackend_APIErrorFieldIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(generateResponse{Error: "model not found"})
	}))
	defer srv.Close()
	b := NewCloudKeyBackend("cloud-key", srv.URL, "k", "m", srv.Client())
	_, err := b.Complete(context.Background(), "p")
	if err == nil {
		t.Fatal("expected error on api error field, got nil")
	}
	if !strings.Contains(err.Error(), "model not found") {
		t.Errorf("error = %v, want it to carry the api error message", err)
	}
}

// TestAnalyst_HTTPFailoverEndToEnd wires the analyst over a FAILING OAuth-GPT
// httptest server (primary) and a HEALTHY cloud-key httptest server (secondary),
// proving real-HTTP-shaped failover is recorded and the secondary's View comes
// back — with no live network (httptest only).
func TestAnalyst_HTTPFailoverEndToEnd(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	defer down.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(generateResponse{Response: "LEAN: neutral\nRATIONALE: chop\n"})
	}))
	defer up.Close()

	primary := NewOAuTHGPTBackend("oauth-gpt", down.URL, "tok", "m", down.Client())
	secondary := NewCloudKeyBackend("cloud-key", up.URL, "key", "m", up.Client())
	log := &capturingLogger{}
	a, err := New(testPlaybook(t), log, primary, secondary)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v, err := a.Analyze(context.Background(), testContext(), nil)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if v.Backend != "cloud-key" {
		t.Errorf("View.Backend = %q, want cloud-key (failover)", v.Backend)
	}
	if !v.Downgraded() || len(v.Failovers) != 1 || v.Failovers[0].From != "oauth-gpt" {
		t.Errorf("failover not recorded as expected: %+v", v.Failovers)
	}
	if !strings.Contains(strings.ToLower(log.joined()), "failing over") {
		t.Errorf("failover not logged; log:\n%s", log.joined())
	}
}
