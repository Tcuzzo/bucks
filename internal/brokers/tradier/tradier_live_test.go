//go:build tradier_live

// This live smoke test is NOT compiled by the default test suite — it only builds
// under `-tags tradier_live`. It hits Tradier's real SANDBOX API (paper trades),
// so it must never run in the loop/CI default path.
//
// The operator runs it once a sandbox token + account are provided:
//
//	TRADIER_SANDBOX_TOKEN=... TRADIER_ACCOUNT_ID=... \
//	  go test -tags tradier_live ./internal/brokers/tradier/ -run TestLive -v
//
// It does only READ-ONLY calls (Account + Quote) — no live order is placed.
package tradier

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestLive_AccountAndQuote(t *testing.T) {
	token := os.Getenv("TRADIER_SANDBOX_TOKEN")
	acct := os.Getenv("TRADIER_ACCOUNT_ID")
	if token == "" || acct == "" {
		t.Fatalf("TRADIER_SANDBOX_TOKEN and TRADIER_ACCOUNT_ID must be set to run the live smoke test " +
			"(this file only compiles under -tags tradier_live; it is not part of the default suite)")
	}

	b, err := New(Config{
		AccessToken: token,
		BaseURL:     "https://sandbox.tradier.com",
		AccountID:   acct,
	}, &http.Client{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("new live tradier: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := b.Account(ctx); err != nil {
		t.Fatalf("live account: %v", err)
	}
	q, err := b.Quote(ctx, "AAPL")
	if err != nil {
		t.Fatalf("live quote: %v", err)
	}
	if q.Last.IsZero() {
		t.Fatalf("live quote returned zero last price")
	}
}
