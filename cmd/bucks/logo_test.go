package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRunLogo_PrintsColoredBanner proves `bucks logo` writes the BUCKS wordmark — with
// the block art AND ANSI color escapes (proving it's the colored render) — and returns
// nil. Driven on the real runLogo entry point with an injected buffer (offline).
func TestRunLogo_PrintsColoredBanner(t *testing.T) {
	var out bytes.Buffer
	if err := runLogo(&out); err != nil {
		t.Fatalf("runLogo: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "██████") {
		t.Errorf("logo output missing the block wordmark art; output:\n%s", got)
	}
	if !strings.Contains(got, "\x1b[") {
		t.Errorf("logo output missing ANSI color escapes — banner not colored; output:\n%s", got)
	}
	if !strings.Contains(got, "the 8-point buck — a trader, not an assistant") {
		t.Errorf("logo output missing the tagline; output:\n%s", got)
	}
}

// TestRunLogo_DispatchedFromRun proves the actual CLI dispatch reaches the logo path:
// `bucks logo` returns nil (exit 0). Its alias `bucks mascot` is dispatched too.
func TestRunLogo_DispatchedFromRun(t *testing.T) {
	for _, name := range []string{"logo", "mascot"} {
		if err := run([]string{name}); err != nil {
			t.Fatalf("`bucks %s` dispatch errored: %v", name, err)
		}
	}
}
