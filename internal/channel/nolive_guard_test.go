package channel

import (
	"os"
	"strings"
	"testing"
)

// TestTelegramLiveIsBuildTagged proves the live Telegram implementation is fenced
// behind the `telegram_live` build tag, so it is NEVER compiled into the default
// test suite. It asserts the source file carries the `//go:build telegram_live`
// constraint as its first non-empty directive. This is the structural half of the
// "no live call in the default suite" guarantee (the behavioral half is in
// nolive_test.go); together they prove a normal `go test ./...` cannot reach the
// network from this package.
func TestTelegramLiveIsBuildTagged(t *testing.T) {
	const file = "telegram_live.go"
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	src := string(data)
	if !strings.Contains(src, "//go:build telegram_live") {
		t.Fatalf("%s must carry the `//go:build telegram_live` constraint so it is excluded from the default build", file)
	}
	// The live transport types must live ONLY in the tagged file.
	if !strings.Contains(src, "type TelegramChannel struct") {
		t.Fatalf("TelegramChannel must be defined in the build-tagged %s", file)
	}
	// And no OTHER (default-compiled) .go file may define the live type.
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
			t.Fatalf("TelegramChannel leaked into default-compiled file %s — it must stay build-tagged", name)
		}
	}
}
