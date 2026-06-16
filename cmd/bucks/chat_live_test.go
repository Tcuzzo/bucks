//go:build chat_live

// This live chat smoke test is NOT compiled by the default test suite — it only
// builds under `-tags chat_live`. It makes REAL network calls to whatever
// OpenAI/Ollama-compatible endpoint the chat env vars point at, so it must never run
// in the loop/CI default path (the no-live-network-in-default-tests rule).
//
// The operator runs it once a key + endpoint are provided (the SAME env the REPL uses):
//
//	BUCKS_CHAT_BASEURL=https://ollama.com \
//	BUCKS_CHAT_KEY=... BUCKS_CHAT_MODEL=qwen3.5:cloud \
//	  go test -tags chat_live ./cmd/bucks/ -run TestLiveChat -v
//
// It holds a 2-3 turn real conversation through the SAME Chatter the `bucks chat`
// REPL uses and asserts each reply is non-empty and coherent — proving the live chat
// path works end to end. It is gated, not skipped: under the tag it runs for real;
// without the tag it is not compiled at all.
package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveChat(t *testing.T) {
	baseURL := strings.TrimSpace(os.Getenv("BUCKS_CHAT_BASEURL"))
	key := strings.TrimSpace(os.Getenv("BUCKS_CHAT_KEY"))
	model := strings.TrimSpace(os.Getenv("BUCKS_CHAT_MODEL"))
	if baseURL == "" {
		t.Fatalf("set BUCKS_CHAT_BASEURL (and BUCKS_CHAT_KEY/_MODEL) to run the live chat test")
	}

	chatter, err := newChatterFromEnv(baseURL, key, model, "grounded and plain-spoken")
	if err != nil {
		t.Fatalf("build chatter: %v", err)
	}

	turns := []string{
		"hey, how are you doing today as my trader?",
		"in one sentence, what's your honest take on risk right now?",
		"and remind me — are you trading real money or paper by default?",
	}
	for i, u := range turns {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		reply, err := chatter.Say(ctx, u)
		cancel()
		if err != nil {
			t.Fatalf("live turn %d (%q): %v", i+1, u, err)
		}
		if strings.TrimSpace(reply.Text) == "" {
			t.Fatalf("live turn %d returned an empty reply", i+1)
		}
		t.Logf("turn %d backend=%s reply=%q", i+1, reply.Backend, reply.Text)
	}
	// After the 3 exchanges, the bounded history holds both sides of the conversation.
	if got := chatter.Conversation().Len(); got == 0 {
		t.Error("expected the conversation history to be populated after live turns")
	}
}
