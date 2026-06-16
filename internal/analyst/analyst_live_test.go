//go:build analyst_live

// This live analyst smoke test is NOT compiled by the default test suite — it
// only builds under `-tags analyst_live`. It makes a REAL network call to an
// OpenAI-compatible chat-completions endpoint, so it must never run in the
// loop/CI default path (the no-live-network-in-default-tests rule).
//
// The operator runs it once a key + endpoint are provided:
//
//	ANALYST_LIVE_BASE_URL=https://api.openai.com/v1 \
//	ANALYST_LIVE_TOKEN=sk-... ANALYST_LIVE_MODEL=gpt-4o-mini \
//	  go test -tags analyst_live ./internal/analyst/ -run TestLiveAnalyze -v
//
// It is read-only (it only asks the model to read a fabricated market context)
// and asserts a non-empty View comes back from the named backend — proving the
// real OAuth path works end to end. It is gated, not skipped: under the tag it
// runs for real; without the tag it is not compiled at all.
package analyst

import (
	"context"
	"os"
	"testing"
	"time"

	"bucks/internal/playbook"
)

func liveEnv(t *testing.T) (baseURL, token, model string) {
	t.Helper()
	baseURL = os.Getenv("ANALYST_LIVE_BASE_URL")
	token = os.Getenv("ANALYST_LIVE_TOKEN")
	model = os.Getenv("ANALYST_LIVE_MODEL")
	if baseURL == "" || token == "" {
		t.Fatalf("set ANALYST_LIVE_BASE_URL and ANALYST_LIVE_TOKEN to run the live analyst test")
	}
	if model == "" {
		model = "gpt-4o-mini"
	}
	return baseURL, token, model
}

func TestLiveAnalyze(t *testing.T) {
	baseURL, token, model := liveEnv(t)

	pb, err := playbook.BuildPlaybook(map[string]string{
		playbook.KeyRiskTolerance: "moderate",
		playbook.KeyCapital:       "10000",
		playbook.KeyStyle:         "swing",
		playbook.KeyMaxDrawdown:   "0.20",
	})
	if err != nil {
		t.Fatalf("build playbook: %v", err)
	}

	backend := NewOAuTHGPTBackend("oauth-gpt", baseURL, token, model, nil)
	a, err := New(pb, nil, backend)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	v, err := a.Analyze(ctx, MarketContext{
		Symbol:  "AAPL",
		Summary: "uptrend pulling back to the 20-day moving average",
		Notes:   []string{"RSI=58", "ATR(14)=2.31"},
	}, map[string]string{"atr14": "2.31", "rsi": "58"})
	if err != nil {
		t.Fatalf("live Analyze: %v", err)
	}
	if v.Backend != "oauth-gpt" {
		t.Errorf("View.Backend = %q, want oauth-gpt", v.Backend)
	}
	if v.Rationale == "" && v.Lean == LeanNeutral {
		t.Errorf("live view looks empty: %+v", v)
	}
	t.Logf("live view: lean=%s backend=%s rationale=%q", v.Lean, v.Backend, v.Rationale)
}
