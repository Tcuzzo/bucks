package analyst

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestCodexBackendReturnsCompletion proves the codex-backed brain returns the runner's
// output (trimmed) and is named "oauth-gpt" — the stable id the dashboard/View already use
// for the ChatGPT path. The runner is injected, so this exercises the real backend logic
// with NO codex installed and NO network.
func TestCodexBackendReturnsCompletion(t *testing.T) {
	b := NewCodexBackend("", "", func(ctx context.Context, prompt string) (string, error) {
		return "  bullish — momentum is strong\n", nil
	})
	if b.Name() != "oauth-gpt" {
		t.Errorf("Name() = %q, want oauth-gpt (default)", b.Name())
	}
	out, err := b.Complete(context.Background(), "read AAPL")
	if err != nil {
		t.Fatalf("Complete errored: %v", err)
	}
	if out != "bullish — momentum is strong" {
		t.Errorf("Complete = %q, want the trimmed runner output", out)
	}
}

// TestCodexBackendPassesPromptToRunner proves the prompt reaches the runner verbatim (no
// mangling) — the model must see the exact analysis prompt the analyst built.
func TestCodexBackendPassesPromptToRunner(t *testing.T) {
	var seen string
	b := NewCodexBackend("oauth-gpt", "gpt-5.5", func(ctx context.Context, prompt string) (string, error) {
		seen = prompt
		return "neutral", nil
	})
	const want = "OWNER PLAYBOOK:\n- style: hold\nMARKET CONTEXT: AAPL"
	if _, err := b.Complete(context.Background(), want); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if seen != want {
		t.Errorf("runner saw %q, want the prompt verbatim %q", seen, want)
	}
}

// TestCodexBackendEmptyOutputIsError proves an empty completion is an ERROR (so the analyst
// fails over), never a fabricated/blank answer presented as real.
func TestCodexBackendEmptyOutputIsError(t *testing.T) {
	b := NewCodexBackend("oauth-gpt", "", func(ctx context.Context, prompt string) (string, error) {
		return "   \n  ", nil
	})
	if _, err := b.Complete(context.Background(), "x"); err == nil {
		t.Error("empty codex output must be an error, not a silent blank completion")
	}
}

// TestCodexBackendRunnerErrorPropagates proves a codex failure (not installed, not logged
// in, quota) surfaces as an error the analyst can fail over on — never swallowed.
func TestCodexBackendRunnerErrorPropagates(t *testing.T) {
	sentinel := errors.New("codex exec: not logged in")
	b := NewCodexBackend("oauth-gpt", "", func(ctx context.Context, prompt string) (string, error) {
		return "", sentinel
	})
	_, err := b.Complete(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("runner error must propagate, got %v", err)
	}
}
