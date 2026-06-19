//go:build alpaca_live

// Real Alpaca PAPER smoke (NO real money — the paper venue). It exercises the PRODUCTION
// brokerFromCreds path against the REAL Alpaca paper API, proving the live-broker wiring
// actually connects and reads a real account — the thing mocks can't prove. Provide your
// Alpaca PAPER keys (from the Alpaca dashboard) and run:
//
//	ALPACA_PAPER_KEY=... ALPACA_PAPER_SECRET=... \
//	  go test -tags alpaca_live ./cmd/bucks/ -run TestLiveAlpacaPaper -v
package main

import (
	"context"
	"os"
	"testing"
	"time"

	"bucks/internal/tui"
)

func TestLiveAlpacaPaper(t *testing.T) {
	key := os.Getenv("ALPACA_PAPER_KEY")
	secret := os.Getenv("ALPACA_PAPER_SECRET")
	if key == "" || secret == "" {
		t.Skip("set ALPACA_PAPER_KEY + ALPACA_PAPER_SECRET to run the real Alpaca paper smoke")
	}

	// The PRODUCTION path: brokerFromCreds builds the real adapter pointed at the real paper API.
	b, err := brokerFromCreds(tui.BrokerCreds{Kind: tui.BrokerAlpacaPaper, Key: key, Secret: secret})
	if err != nil {
		t.Fatalf("brokerFromCreds: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	acct, err := b.Account(ctx)
	if err != nil {
		t.Fatalf("real Alpaca paper Account() via brokerFromCreds failed: %v", err)
	}
	t.Logf("real Alpaca paper account OK — equity=%s cash=%s buying_power=%s",
		acct.Equity, acct.Cash, acct.BuyingPower)
	if acct.Equity.Sign() < 0 {
		t.Errorf("account equity should be >= 0, got %s", acct.Equity)
	}
}
