//go:build codex_live

// This test makes a REAL call to the locally-installed `codex` CLI (ChatGPT via the owner's
// OAuth login). It builds ONLY under -tags codex_live, and is the durable proof that the
// PRODUCTION CodexBackend — its real `codex exec` invocation and stdout parsing — actually
// returns a completion the analyst can parse into a lean. No mock: this is the real brain.
//
//	go test -tags codex_live ./internal/analyst/ -run TestLiveCodexBackend -v
package analyst

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLiveCodexBackend(t *testing.T) {
	if !CodexAvailable() {
		t.Skip("codex CLI not installed / not on PATH — skipping the live codex check")
	}
	b := NewCodexBackend("oauth-gpt", "", nil) // nil runner => the REAL `codex exec`
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	out, err := b.Complete(ctx, "You are a trading analyst. Reply with EXACTLY two lines and nothing else:\nLEAN: bullish\nRATIONALE: live codex smoke")
	if err != nil {
		t.Fatalf("real codex Complete failed: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("real codex returned an empty completion")
	}
	// The whole point: the real answer must parse into a concrete lean the loop can act on.
	v := parseView("TEST", out)
	if v.Lean != LeanBullish {
		t.Errorf("real codex answer did not parse to a bullish lean: got %q (raw: %q)", v.Lean, out)
	}
	t.Logf("real codex OK — lean=%s rationale=%q", v.Lean, v.Rationale)
}
