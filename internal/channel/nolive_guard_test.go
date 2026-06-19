package channel

import (
	"os"
	"strings"
	"testing"
)

// TestTelegramChannelDefaultCompiledAndDefinedOnce documents the un-fenced state of
// the live Telegram channel. It used to be behind a `telegram_live` build tag; that
// tag was removed so the real channel actually ships in the binary and runs under
// `bucks --daemon`. The "no live call in the default test suite" guarantee is now
// purely BEHAVIORAL — the dial-blocker in nolive_test.go (TestMain) counts and refuses
// every real outbound dial, so a normal `go test ./...` still cannot reach a live
// operator even though the live code is compiled in. The hermetic live tests reach a
// local httptest server through their own transport, never http.DefaultTransport.
//
// This test pins two things: (1) the type is part of the DEFAULT build (the
// compile-time assertion below only builds if TelegramChannel exists without any tag),
// and (2) it is defined in exactly one source file (no accidental duplication).
func TestTelegramChannelDefaultCompiledAndDefinedOnce(t *testing.T) {
	// Compiles only if TelegramChannel is in the default build (no build tag).
	var _ Channel = (*TelegramChannel)(nil)

	const file = "telegram_live.go"
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == file || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(name)
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		if strings.Contains(string(b), "type TelegramChannel struct") {
			t.Fatalf("TelegramChannel defined in %s as well as %s — it must be defined once", name, file)
		}
	}
}
