package main

import (
	"path/filepath"
	"strings"
	"testing"

	"bucks/internal/channel"
	"bucks/internal/tui"
)

// TestIsLiveBrokerCoversEveryRealMoneyVenue pins the real-money classification for EVERY
// broker kind the wizard offers: any venue where an order moves actual funds must be
// classified real-money, so the trade-loop confirmation gate can never be bypassed by
// picking a venue the predicate forgot. Only Alpaca's paper environment is simulated.
func TestIsLiveBrokerCoversEveryRealMoneyVenue(t *testing.T) {
	cases := []struct {
		kind      tui.BrokerKind
		realMoney bool
	}{
		{tui.BrokerAlpacaPaper, false},
		{tui.BrokerAlpacaLive, true},
		{tui.BrokerCoinbase, true},
		{tui.BrokerTradier, true},
	}
	for _, c := range cases {
		if got := isLiveBroker(c.kind); got != c.realMoney {
			t.Errorf("isLiveBroker(%s) = %v, want %v — every venue where an order moves actual funds must be classified real-money", c.kind, got, c.realMoney)
		}
	}
}

// TestBrokerFromCredsWiresRealBaseURLs proves the coinbase/tradier adapters are wired to
// REAL production endpoints and construct successfully from saved creds (construction is
// offline — only a later call hits the wire). An empty BaseURL is a construction ERROR in
// both adapters, so an empty default here means the venue can never be traded at all.
func TestBrokerFromCredsWiresRealBaseURLs(t *testing.T) {
	if !strings.HasPrefix(coinbaseBaseURL, "https://") {
		t.Errorf("coinbaseBaseURL = %q, want a real https production endpoint", coinbaseBaseURL)
	}
	if !strings.HasPrefix(tradierBaseURL, "https://") {
		t.Errorf("tradierBaseURL = %q, want a real https production endpoint", tradierBaseURL)
	}
	if _, err := brokerFromCreds(tui.BrokerCreds{Kind: tui.BrokerCoinbase, Key: "organizations/x/apiKeys/y", Secret: "fake-pem-not-a-real-secret"}); err != nil {
		t.Errorf("coinbase creds must construct offline against the default endpoint: %v", err)
	}
	if _, err := brokerFromCreds(tui.BrokerCreds{Kind: tui.BrokerTradier, Key: "fake-access-token", Secret: "ACCT-12345"}); err != nil {
		t.Errorf("tradier creds must construct offline against the default endpoint: %v", err)
	}
}

// TestBuildLiveTraderRealMoneyVenuesNeedConfirm is the loop-level real-money safety proof
// for the NON-Alpaca venues: coinbase/tradier creds WITHOUT the explicit per-session
// confirmation (`bucks --live`) must NOT start the trade loop — no trader, no broker,
// nothing that can place a real order — exactly like alpaca-live.
func TestBuildLiveTraderRealMoneyVenuesNeedConfirm(t *testing.T) {
	for _, kind := range []tui.BrokerKind{tui.BrokerCoinbase, tui.BrokerTradier} {
		t.Run(string(kind), func(t *testing.T) {
			r := validSetupResult(t)
			r.Brokers = []tui.BrokerCreds{{Kind: kind, Key: "real-money-key-12345", Secret: "real-money-secret-67890"}}
			res, err := buildLiveTrader(r, filepath.Join(t.TempDir(), "bucks.yaml"),
				channel.NewMockChannel(), false, nil)
			if err != nil {
				t.Fatalf("buildLiveTrader: %v", err)
			}
			if res.Trader != nil {
				t.Fatalf("SAFETY VIOLATION: %s (real money) started the loop WITHOUT a session confirmation", kind)
			}
			if res.LiveActive {
				t.Fatalf("SAFETY VIOLATION: %s live-active without confirmation", kind)
			}
			if !strings.Contains(strings.ToLower(res.Reason), "live") {
				t.Errorf("reason should explain it stayed safe pending live confirmation, got %q", res.Reason)
			}
		})
	}
}
