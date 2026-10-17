package main

import (
	"path/filepath"
	"strings"
	"testing"

	"bucks/internal/channel"
	"bucks/internal/tui"
)

// TestIsLiveBrokerCoversEveryRealMoneyVenue pins the real-money classification for EVERY
// recognized broker kind: any venue where an order moves actual funds must be
// classified real-money, so the construction refusal cannot be bypassed.
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

// TestBrokerFromCredsWiresRealBaseURLs pins the internal adapter URL and proves
// legacy Coinbase configs are rejected as unsupported.
func TestBrokerFromCredsWiresRealBaseURLs(t *testing.T) {
	if !strings.HasPrefix(tradierBaseURL, "https://") {
		t.Errorf("tradierBaseURL = %q, want a real https production endpoint", tradierBaseURL)
	}
	if _, err := brokerFromCreds(tui.BrokerCreds{Kind: tui.BrokerCoinbase, Key: "organizations/x/apiKeys/y", Secret: "fake-pem-not-a-real-secret"}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "unsupported") {
		t.Errorf("legacy Coinbase config must fail loudly as unsupported, got: %v", err)
	}
	if _, err := brokerFromCreds(tui.BrokerCreds{Kind: tui.BrokerTradier, Key: "fake-access-token", Secret: "ACCT-12345"}); err != nil {
		t.Errorf("tradier creds must construct offline against the default endpoint: %v", err)
	}
}

// TestBuildLiveTraderDisablesOtherRealMoneyVenues proves legacy Coinbase and
// Tradier configurations cannot construct a loop. Coinbase is retained
// as a recognized legacy kind so saved configs fail closed at this earlier gate.
func TestBuildLiveTraderDisablesOtherRealMoneyVenues(t *testing.T) {
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
				t.Fatalf("SAFETY VIOLATION: %s (real money) started the loop", kind)
			}
			if res.LiveActive {
				t.Fatalf("SAFETY VIOLATION: refused %s setup reported live-active", kind)
			}
			if !strings.Contains(strings.ToLower(res.Reason), "exit path") {
				t.Errorf("reason should explain why real-money construction is disabled, got %q", res.Reason)
			}
		})
	}
}
