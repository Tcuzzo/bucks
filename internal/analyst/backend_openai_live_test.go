//go:build nemotron_live

// This live NVIDIA-NIM smoke test is NOT compiled by the default test suite — it
// only builds under `-tags nemotron_live`. It makes a REAL network call to NVIDIA
// NIM's OpenAI-compatible chat-completions endpoint with the free Nemotron Nano
// model, so it must never run in the loop/CI default path (the
// no-live-network-in-default-tests rule).
//
// The END-USER runs it once they have their own free key (get one, no credit card,
// at build.nvidia.com):
//
//	NVIDIA_API_KEY=nvapi-... \
//	  go test -tags nemotron_live ./internal/analyst/ -run TestLiveNemotron -v
//
// Optional NEMOTRON_LIVE_MODEL overrides the model (defaults to the profile's free
// Nano model). It is a single read-only 1-shot call asserting a non-empty answer
// comes back — proving the free OpenAI-compatible path works end to end against the
// real provider. It is gated, not skipped: under the tag it runs for real; without
// the tag it is not compiled at all.
package analyst

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveNemotron(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("NVIDIA_API_KEY"))
	if key == "" {
		t.Fatalf("set NVIDIA_API_KEY (a free nvapi-... key from build.nvidia.com) to run the live Nemotron test")
	}

	profile, err := ProviderProfileByName(ProviderNvidia)
	if err != nil {
		t.Fatalf("nvidia profile: %v", err)
	}
	model := strings.TrimSpace(os.Getenv("NEMOTRON_LIVE_MODEL")) // empty => profile default (free Nano)

	backend := NewOpenAICompatBackend(profile, key, model)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	out, err := backend.Complete(ctx, "In one short sentence, what is a stop-loss order?")
	if err != nil {
		t.Fatalf("live Nemotron Complete: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("live Nemotron returned an empty completion")
	}
	t.Logf("live Nemotron (%s) replied: %q", backend.Name(), out)
}
