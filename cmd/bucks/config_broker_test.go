package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"bucks/internal/tui"
)

// acctServer is a hermetic Alpaca trading-API stand-in: it records that it was hit and the
// API-key header it received, and returns a minimal valid account so Account() succeeds.
func acctServer(hit *bool, gotKey *string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hit = true
		if k := r.Header.Get("APCA-API-KEY-ID"); k != "" && gotKey != nil {
			*gotKey = k
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"acct","status":"ACTIVE","currency":"USD","cash":"1000","equity":"1000","buying_power":"1000"}`))
	}))
}

// swapAlpacaURLs points the paper/live/data endpoints at test servers and returns a restore.
func swapAlpacaURLs(paper, live, data string) func() {
	pp, pl, pd := alpacaPaperBaseURL, alpacaLiveBaseURL, alpacaDataBaseURL
	alpacaPaperBaseURL, alpacaLiveBaseURL, alpacaDataBaseURL = paper, live, data
	return func() { alpacaPaperBaseURL, alpacaLiveBaseURL, alpacaDataBaseURL = pp, pl, pd }
}

// TestBrokerFromCredsPaperNeverHitsLiveVenue is THE safety proof: an alpaca-PAPER config must
// reach ONLY the paper trading API, never the live (real-money) endpoint, and must carry the
// owner's saved key.
func TestBrokerFromCredsPaperNeverHitsLiveVenue(t *testing.T) {
	var paperHit, liveHit bool
	var gotKey string
	paperSrv := acctServer(&paperHit, &gotKey)
	defer paperSrv.Close()
	liveSrv := acctServer(&liveHit, nil)
	defer liveSrv.Close()
	defer swapAlpacaURLs(paperSrv.URL, liveSrv.URL, paperSrv.URL)()

	b, err := brokerFromCreds(tui.BrokerCreds{Kind: tui.BrokerAlpacaPaper, Key: "PAPER-KEY", Secret: "PAPER-SECRET"})
	if err != nil {
		t.Fatalf("brokerFromCreds: %v", err)
	}
	if _, err := b.Account(context.Background()); err != nil {
		t.Fatalf("Account: %v", err)
	}
	if !paperHit {
		t.Error("paper config did not reach the paper venue")
	}
	if liveHit {
		t.Fatal("SAFETY VIOLATION: a paper config reached the LIVE (real-money) venue")
	}
	if gotKey != "PAPER-KEY" {
		t.Errorf("saved key not sent to the venue: got %q, want PAPER-KEY", gotKey)
	}
}

// TestBrokerFromCredsLiveUsesLiveVenue proves an alpaca-live config reaches the live endpoint
// (so real-money trading actually connects when explicitly armed).
func TestBrokerFromCredsLiveUsesLiveVenue(t *testing.T) {
	var paperHit, liveHit bool
	paperSrv := acctServer(&paperHit, nil)
	defer paperSrv.Close()
	liveSrv := acctServer(&liveHit, nil)
	defer liveSrv.Close()
	defer swapAlpacaURLs(paperSrv.URL, liveSrv.URL, paperSrv.URL)()

	b, err := brokerFromCreds(tui.BrokerCreds{Kind: tui.BrokerAlpacaLive, Key: "LIVE-KEY", Secret: "LIVE-SECRET"})
	if err != nil {
		t.Fatalf("brokerFromCreds: %v", err)
	}
	if _, err := b.Account(context.Background()); err != nil {
		t.Fatalf("Account: %v", err)
	}
	if !liveHit {
		t.Error("live config did not reach the live venue")
	}
	if paperHit {
		t.Error("live config unexpectedly hit the paper venue")
	}
}

// TestBrokerFromCredsUnknownKindErrors proves an unknown venue is refused — never silently traded.
func TestBrokerFromCredsUnknownKindErrors(t *testing.T) {
	if _, err := brokerFromCreds(tui.BrokerCreds{Kind: "mystery-exchange", Key: "k", Secret: "s"}); err == nil {
		t.Fatal("unknown broker kind must error, not build a broker")
	}
}

// TestIsLiveBroker pins the real-money classification the loop gate relies on.
func TestIsLiveBroker(t *testing.T) {
	if !isLiveBroker(tui.BrokerAlpacaLive) {
		t.Error("alpaca-live must be classified as a real-money venue")
	}
	if isLiveBroker(tui.BrokerAlpacaPaper) {
		t.Error("alpaca-paper must NOT be classified as real-money")
	}
}
