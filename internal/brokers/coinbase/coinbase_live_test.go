//go:build coinbase_live

// This live smoke test is NOT compiled by the default test suite — it only builds
// under `-tags coinbase_live`. It hits Coinbase Advanced Trade's real REST API, so
// it must never run in the loop/CI default path.
//
// The operator runs it once CDP credentials are provided:
//
//	COINBASE_API_KEY_NAME=... COINBASE_API_SECRET=... \
//	  go test -tags coinbase_live ./internal/brokers/coinbase/ -run TestLive -v
//
// It does only READ-ONLY calls (Account + Quote) — no live order is placed — so it
// can never move money. The real ES256 JWT minter is wired in a later slice; this
// smoke test exercises the auto-refresh path against the live endpoint.
package coinbase

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestLive_AccountAndQuote(t *testing.T) {
	name := os.Getenv("COINBASE_API_KEY_NAME")
	secret := os.Getenv("COINBASE_API_SECRET")
	if name == "" || secret == "" {
		t.Fatalf("COINBASE_API_KEY_NAME and COINBASE_API_SECRET must be set to run the live smoke test " +
			"(this file only compiles under -tags coinbase_live; it is not part of the default suite)")
	}

	cb, err := New(Config{
		APIKeyName: name,
		APISecret:  secret,
		BaseURL:    "https://api.coinbase.com",
		TokenTTL:   2 * time.Minute,
	}, &http.Client{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("new live coinbase: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := cb.Account(ctx); err != nil {
		t.Fatalf("live account: %v", err)
	}
	q, err := cb.Quote(ctx, "BTC-USD")
	if err != nil {
		t.Fatalf("live quote: %v", err)
	}
	if q.Last.IsZero() {
		t.Fatalf("live quote returned zero last price")
	}
}
