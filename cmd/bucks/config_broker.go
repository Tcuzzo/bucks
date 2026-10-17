package main

import (
	"fmt"
	"net/http"

	"bucks/internal/brokers"
	"bucks/internal/brokers/alpaca"
	"bucks/internal/brokers/tradier"
	"bucks/internal/tui"
)

// Venue endpoints. These are package VARS (not consts) ONLY so the default test suite
// can point every adapter at an httptest server; production always uses the real endpoints.
// The paper/live split is SAFETY-CRITICAL: a paper config must never reach the live API.
// The Tradier adapter requires a non-empty BaseURL, so its production root lives here.
var (
	alpacaPaperBaseURL = "https://paper-api.alpaca.markets"
	alpacaLiveBaseURL  = "https://api.alpaca.markets"
	alpacaDataBaseURL  = "https://data.alpaca.markets"
	tradierBaseURL     = "https://api.tradier.com"
)

// brokerHTTPClient is the HTTP client the Tradier adapter uses (Alpaca's SDK manages
// its own). nil in production (default client); a test injects one.
var brokerHTTPClient *http.Client

// brokerFromCreds builds a live brokers.Broker from the owner's saved broker credentials. The
// KIND selects the venue and, for Alpaca, the safety-critical trading endpoint:
//   - alpaca-paper -> the PAPER API (simulated; no real money),
//   - alpaca-live  -> the LIVE API (real money),
//   - tradier -> its API, from the saved key/secret.
//
// The saved key/secret are passed verbatim. An unknown/empty kind is a clear error — BUCKS
// never trades against an unknown venue. Construction is offline; only a later call hits the wire.
func brokerFromCreds(c tui.BrokerCreds) (brokers.Broker, error) {
	switch c.Kind {
	case tui.BrokerAlpacaPaper:
		return alpaca.New(alpaca.Config{KeyID: c.Key, Secret: c.Secret, BaseURL: alpacaPaperBaseURL, DataBaseURL: alpacaDataBaseURL})
	case tui.BrokerAlpacaLive:
		return alpaca.New(alpaca.Config{KeyID: c.Key, Secret: c.Secret, BaseURL: alpacaLiveBaseURL, DataBaseURL: alpacaDataBaseURL})
	case tui.BrokerCoinbase:
		return nil, fmt.Errorf("config: Coinbase is unsupported because bucks has no production ES256 signer")
	case tui.BrokerTradier:
		return tradier.New(tradier.Config{AccessToken: c.Key, AccountID: c.Secret, BaseURL: tradierBaseURL}, brokerHTTPClient)
	default:
		return nil, fmt.Errorf("config: unknown broker kind %q — refusing to build a broker", c.Kind)
	}
}

// isLiveBroker reports whether a saved broker kind trades real money, so the trade
// loop can refuse it before adapter construction. The classification delegates to
// tui.BrokerKind.IsRealMoney, the single source of truth.
func isLiveBroker(kind tui.BrokerKind) bool {
	return kind.IsRealMoney()
}
