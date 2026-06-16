//go:build probe_live

// This live capability-probe smoke test is NOT compiled by the default suite — it
// only builds under `-tags probe_live`. It runs the read-only-first probe against
// a REAL broker base URL (e.g. a sandbox host) so the operator can confirm the
// probe discovers + classifies a live API without ever issuing a write.
//
//	PROBE_BASE_URL=https://sandbox.tradier.com \
//	  go test -tags probe_live ./internal/probe/ -run TestLiveProbe -v
//
// The probe is physically read-only (GET/HEAD/OPTIONS only), and the returned
// write gate is asserted LOCKED — so this can never place an order.
package probe

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestLiveProbe_ReadOnlyAndGateLocked(t *testing.T) {
	base := os.Getenv("PROBE_BASE_URL")
	if base == "" {
		t.Fatalf("PROBE_BASE_URL must be set to run the live probe smoke test " +
			"(this file only compiles under -tags probe_live; it is not part of the default suite)")
	}

	p := NewProbe(&http.Client{Timeout: 15 * time.Second}, base, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := p.Run(ctx)
	if err != nil {
		// A venue without a published spec returns ErrSpecNotFound — that is a
		// legitimate outcome, not a crash; surface it for the operator.
		t.Fatalf("live probe: %v", err)
	}
	if len(res.Manifest.Capabilities) == 0 {
		t.Fatalf("live probe found no capabilities")
	}
	// Writes MUST stay locked after a probe — paper-run + operator-confirm pending.
	if err := res.Gate.AssertWriteAllowed(); err == nil {
		t.Fatalf("write gate unlocked after a bare probe — must stay locked")
	}
	t.Logf("probed %d capabilities (%d read / %d write); gate: %s",
		len(res.Manifest.Capabilities), len(res.Manifest.Reads()), len(res.Manifest.Writes()), res.Gate.Status())
}
