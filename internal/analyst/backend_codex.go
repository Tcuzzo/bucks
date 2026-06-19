package analyst

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// CodexRunner executes a single codex completion for prompt and returns the model's text.
// It is the injectable seam that keeps CodexBackend testable with NO codex installed and NO
// network: the default suite passes a fake runner; production uses defaultCodexRunner.
type CodexRunner func(ctx context.Context, prompt string) (string, error)

// CodexBackend drives ChatGPT through the locally-installed `codex` CLI using the owner's
// existing ChatGPT login (the OAuth session codex manages). It needs NO API key — that is
// the "sign in with ChatGPT, no key" path. It only works where codex is installed AND
// logged in (see CodexAvailable); callers fall back to the free brain otherwise, so a
// customer without codex is never stranded.
type CodexBackend struct {
	name  string
	model string
	run   CodexRunner
}

// NewCodexBackend builds a codex-backed analyst backend. name defaults to "oauth-gpt" — the
// stable id the View/dashboard already use for the ChatGPT path, so wiring codex in does not
// change what the owner sees. model is the codex model id; empty means "use codex's own
// configured default", the most portable choice for a customer's install. A nil runner uses
// the real codex CLI; tests inject a fake runner.
func NewCodexBackend(name, model string, runner CodexRunner) *CodexBackend {
	if name == "" {
		name = "oauth-gpt"
	}
	if runner == nil {
		runner = defaultCodexRunner(model)
	}
	return &CodexBackend{name: name, model: model, run: runner}
}

// Name is the stable identifier recorded in the View and the failover log.
func (b *CodexBackend) Name() string { return b.name }

// Complete runs the prompt through codex and returns the trimmed completion. An empty
// completion or a runner error returns an error (which makes the analyst fail over to the
// next backend) — it never fabricates a completion.
func (b *CodexBackend) Complete(ctx context.Context, prompt string) (string, error) {
	out, err := b.run(ctx, prompt)
	if err != nil {
		return "", fmt.Errorf("%s: %w", b.name, err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("%s: empty completion", b.name)
	}
	return out, nil
}

// CodexAvailable reports whether the codex CLI is installed on PATH. The OAuth-GPT brain is
// built only when this is true; otherwise the wizard routes the owner to the free brain so a
// customer without codex always has a working path.
func CodexAvailable() bool {
	_, err := exec.LookPath("codex")
	return err == nil
}

// defaultCodexRunner runs `codex exec` headless and returns its stdout. The prompt is passed
// as the task argument (exec.Command does not use a shell, so a multi-line prompt is safe
// with no quoting). read-only sandbox + skip-git so it runs anywhere; the model flag is sent
// only when set (empty -> codex's configured default, maximally portable for a customer).
func defaultCodexRunner(model string) CodexRunner {
	return func(ctx context.Context, prompt string) (string, error) {
		args := []string{"exec", "--skip-git-repo-check", "--sandbox", "read-only"}
		if strings.TrimSpace(model) != "" {
			args = append(args, "-m", model)
		}
		args = append(args, prompt)

		cmd := exec.CommandContext(ctx, "codex", args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = err.Error()
			}
			return "", fmt.Errorf("codex exec failed (is codex installed and logged in?): %s", msg)
		}
		return stdout.String(), nil
	}
}

// compile-time assertion CodexBackend satisfies the Backend contract.
var _ Backend = (*CodexBackend)(nil)
