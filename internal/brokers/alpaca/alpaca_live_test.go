//go:build alpaca_live

// This live paper smoke test is NOT compiled by the default test suite — it only
// builds under `-tags alpaca_live`. It hits Alpaca's real PAPER endpoint
// (paper-api.alpaca.markets), so it must never run in the loop/CI default path.
//
// The operator runs it once paper keys are provided:
//
//	ALPACA_PAPER_KEY=... ALPACA_PAPER_SECRET=... \
//	  go test -tags alpaca_live ./internal/brokers/alpaca/ -run TestLivePaper -v
//
// It deliberately does only READ-ONLY calls (Account + Quote) — no live order is
// placed — so it can never move money or open a position.
package alpaca

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLivePaper_AccountAndQuote(t *testing.T) {
	key := os.Getenv("ALPACA_PAPER_KEY")
	secret := os.Getenv("ALPACA_PAPER_SECRET")
	if key == "" || secret == "" {
		t.Fatalf("ALPACA_PAPER_KEY and ALPACA_PAPER_SECRET must be set to run the live paper smoke test " +
			"(this file only compiles under -tags alpaca_live; it is not part of the default suite)")
	}

	b, err := New(Config{
		KeyID:       key,
		Secret:      secret,
		BaseURL:     "https://paper-api.alpaca.markets",
		DataBaseURL: "https://data.alpaca.markets",
		Feed:        "iex",
	})
	if err != nil {
		t.Fatalf("new live broker: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	acct, err := b.Account(ctx)
	if err != nil {
		t.Fatalf("live account: %v", err)
	}
	if acct.Equity.IsNeg() {
		t.Fatalf("live account equity should not be negative: %s", acct.Equity)
	}
	t.Logf("live paper account: cash=%s equity=%s buying_power=%s",
		acct.Cash, acct.Equity, acct.BuyingPower)

	q, err := b.Quote(ctx, "AAPL")
	if err != nil {
		t.Fatalf("live quote: %v", err)
	}
	if q.Bid.IsNeg() || q.Ask.IsNeg() {
		t.Fatalf("live quote prices should be non-negative: bid=%s ask=%s", q.Bid, q.Ask)
	}
	t.Logf("live AAPL quote: bid=%s ask=%s last=%s at=%s", q.Bid, q.Ask, q.Last, q.Time)
}
