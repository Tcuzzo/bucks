package main

import (
	"fmt"
	"net/http"

	"bucks/internal/brokers"
	"bucks/internal/brokers/alpaca"
	"bucks/internal/brokers/coinbase"
	"bucks/internal/brokers/tradier"
	"bucks/internal/tui"
)

// Alpaca venue endpoints. These are package VARS (not consts) ONLY so the default test suite
// can point every adapter at an httptest server; production always uses the real endpoints.
// The paper/live split is SAFETY-CRITICAL: a paper config must never reach the live API.
var (
	alpacaPaperBaseURL = "https://paper-api.alpaca.markets"
	alpacaLiveBaseURL  = "https://api.alpaca.markets"
	alpacaDataBaseURL  = "https://data.alpaca.markets"
	coinbaseBaseURL    = "" // empty -> the coinbase adapter's own default
	tradierBaseURL     = "" // empty -> the tradier adapter's own default
)

// brokerHTTPClient is the HTTP client the coinbase/tradier adapters use (alpaca's SDK manages
// its own). nil in production (default client); a test injects one.
var brokerHTTPClient *http.Client

// brokerFromCreds builds a live brokers.Broker from the owner's saved broker credentials. The
// KIND selects the venue and, for Alpaca, the safety-critical trading endpoint:
//   - alpaca-paper -> the PAPER API (simulated; no real money),
//   - alpaca-live  -> the LIVE API (real money),
//   - coinbase / tradier -> their API, from the saved key/secret.
// The saved key/secret are passed verbatim. An unknown/empty kind is a clear error — BUCKS
// never trades against an unknown venue. Construction is offline; only a later call hits the wire.
func brokerFromCreds(c tui.BrokerCreds) (brokers.Broker, error) {
	switch c.Kind {
	case tui.BrokerAlpacaPaper:
		return alpaca.New(alpaca.Config{KeyID: c.Key, Secret: c.Secret, BaseURL: alpacaPaperBaseURL, DataBaseURL: alpacaDataBaseURL})
	case tui.BrokerAlpacaLive:
		return alpaca.New(alpaca.Config{KeyID: c.Key, Secret: c.Secret, BaseURL: alpacaLiveBaseURL, DataBaseURL: alpacaDataBaseURL})
	case tui.BrokerCoinbase:
		return coinbase.New(coinbase.Config{APIKeyName: c.Key, APISecret: c.Secret, BaseURL: coinbaseBaseURL}, brokerHTTPClient)
	case tui.BrokerTradier:
		return tradier.New(tradier.Config{AccessToken: c.Key, AccountID: c.Secret, BaseURL: tradierBaseURL}, brokerHTTPClient)
	default:
		return nil, fmt.Errorf("config: unknown broker kind %q — refusing to build a broker", c.Kind)
	}
}

// isLiveBroker reports whether a saved broker kind trades REAL money, so the trade-loop wiring
// can require an explicit per-session confirmation before ever using it. Paper venues are safe
// to run unattended; a real-money venue is not.
func isLiveBroker(kind tui.BrokerKind) bool {
	return kind == tui.BrokerAlpacaLive
}
