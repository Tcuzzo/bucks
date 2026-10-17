package understanding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bucks/internal/analyst"
)

// reasoningReply mirrors a REASONING model's OpenAI-shaped response at the moment
// it runs out of budget: it has spent every token THINKING, so `content` comes back
// EMPTY while `reasoning` is full of chain-of-thought. This is the exact wire shape
// that broke a sibling project's grader: the caller substituted `content or reasoning`, fed prose to a
// JSON parser, and reported a fabricated passing score.
const reasoningReply = `{
  "choices": [
    {"message": {
      "role": "assistant",
      "content": "",
      "reasoning": "Let me think. The spec says loadKey rejects empty keys. Looking at the code I can see it does not. I should score spec_adherence 0 and..."
    }}
  ]
}`

// TestGrade_EmptyContentBesideReasoningFailsLoudly is the anti-substitution proof,
// driven through the REAL OpenAI-compatible backend against a fake HTTP transport
// (the outermost seam, and the only thing faked here). The chain-of-thought must
// never be substituted for the missing payload, and the starved call must never
// land on a pass.
func TestGrade_EmptyContentBesideReasoningFailsLoudly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reasoningReply))
	}))
	defer srv.Close()

	backend := analyst.NewOpenAICompatBackend(
		analyst.ProviderProfile{Name: "nvidia", BaseURL: srv.URL, DefaultModel: "reasoning-test"},
		"nvapi-test-key", "",
		analyst.WithMaxTokens(MinGradingTokens),
	)

	got, err := Grade(context.Background(), []analyst.Backend{backend}, candidateWithBug())
	if err == nil {
		t.Fatal("empty content beside populated reasoning must FAIL LOUDLY, not be substituted")
	}
	if got.Passed() {
		t.Error("a starved grading call Passed() = true — this is the starved-grading bug; it must be false")
	}
	if got.Verdict != VerdictUnavailable {
		t.Errorf("Verdict = %q, want %q", got.Verdict, VerdictUnavailable)
	}
	// The chain-of-thought must not leak into the result as if it were the payload.
	if strings.Contains(got.Rationale(), "Let me think") {
		t.Error("the model's reasoning chain was substituted for the missing content")
	}
	if !strings.Contains(err.Error(), "empty completion") {
		t.Errorf("error = %v, want it to name the empty completion loudly", err)
	}
}

// TestGrade_RequestsGradingTokenBudgetOnTheWire asserts the CALL CONTRACT: what the
// code actually PUTS ON THE WIRE, not a mocked verdict. A grading call must ask for
// at least MinGradingTokens, because the free default brain is a reasoning model
// and a starved budget returns an empty payload.
func TestGrade_RequestsGradingTokenBudgetOnTheWire(t *testing.T) {
	var gotMaxTokens int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotMaxTokens = body.MaxTokens
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":` +
			strconvQuote(gradeJSON(4, 4, 4, 4, 4, "APPROVED", "loadKey")) + `}}]}`))
	}))
	defer srv.Close()

	backend := analyst.NewOpenAICompatBackend(
		analyst.ProviderProfile{Name: "nvidia", BaseURL: srv.URL, DefaultModel: "nano-test"},
		"nvapi-test-key", "",
		analyst.WithMaxTokens(MinGradingTokens),
	)
	if _, err := Grade(context.Background(), []analyst.Backend{backend}, candidateWithBug()); err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if gotMaxTokens < 2000 {
		t.Errorf("grading call sent max_tokens=%d on the wire, want >= 2000", gotMaxTokens)
	}
	if gotMaxTokens != MinGradingTokens {
		t.Errorf("grading call sent max_tokens=%d, want MinGradingTokens=%d", gotMaxTokens, MinGradingTokens)
	}
}

// strconvQuote JSON-quotes s for embedding in a literal response body.
func strconvQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err) // test-only: the input is always a valid string
	}
	return string(b)
}
