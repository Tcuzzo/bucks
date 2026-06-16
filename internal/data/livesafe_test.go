package data

import (
	"context"
	"errors"
	"strings"
	"testing"

	"bucks/internal/kernel"
)

// TestLiveSafe_GatingTable proves AssertLiveTradePath accepts a real-time source
// and REJECTS a backfill source, with the error naming the offending source.
func TestLiveSafe_GatingTable(t *testing.T) {
	tests := []struct {
		name       string
		src        DataSource
		wantReject bool
		wantName   string
	}{
		{
			name:       "realtime exchange feed is live-safe",
			src:        NewRealtimeSource("coinbase-ws", 1),
			wantReject: false,
		},
		{
			name:       "yfinance-style backfill feed is NOT live-safe",
			src:        NewBackfillSource("yfinance", 1),
			wantReject: true,
			wantName:   "yfinance",
		},
		{
			name:       "alphavantage-style backfill feed is NOT live-safe",
			src:        NewBackfillSource("alphavantage", 1),
			wantReject: true,
			wantName:   "alphavantage",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := AssertLiveTradePath(tc.src)
			if tc.wantReject {
				if err == nil {
					t.Fatalf("expected rejection, got nil (a backfill feed reached the live path!)")
				}
				var nls *ErrNotLiveSafe
				if !errors.As(err, &nls) {
					t.Fatalf("error type = %T, want *ErrNotLiveSafe", err)
				}
				if !strings.Contains(err.Error(), tc.wantName) {
					t.Fatalf("error %q does not name the source %q", err.Error(), tc.wantName)
				}
			} else {
				if err != nil {
					t.Fatalf("live-safe source rejected: %v", err)
				}
			}
		})
	}
}

// TestLiveSafe_BackfillCannotWireToLiveTradePath proves enforcement at the REAL
// production constructor: NewLiveIngestor itself refuses a backfill feed (it is the
// live trade path seam, not a test-local wrapper). A backfill source is rejected
// with an error naming it and no ingestor; a real-time source is accepted. It also
// confirms the general NewIngestor STILL allows a backfill source (backtests are
// legitimate) — the gate lives only on the live constructor.
func TestLiveSafe_BackfillCannotWireToLiveTradePath(t *testing.T) {
	k := kernel.New()

	// REAL live trade path constructor: must refuse the backfill feed directly.
	backfill := NewBackfillSource("yfinance", 1)
	in, err := NewLiveIngestor(k, backfill)
	if err == nil {
		t.Fatalf("NewLiveIngestor allowed a backfill feed onto the live trade path")
	}
	var nls *ErrNotLiveSafe
	if !errors.As(err, &nls) {
		t.Fatalf("error type = %T, want *ErrNotLiveSafe from NewLiveIngestor", err)
	}
	if !strings.Contains(err.Error(), "yfinance") {
		t.Fatalf("NewLiveIngestor error %q does not name the source %q", err.Error(), "yfinance")
	}
	if in != nil {
		t.Fatalf("NewLiveIngestor returned an ingestor for a banned backfill feed")
	}

	// REAL live trade path constructor: must ACCEPT a real-time feed.
	realtime := NewRealtimeSource("coinbase-ws", 1)
	in2, err := NewLiveIngestor(k, realtime)
	if err != nil {
		t.Fatalf("NewLiveIngestor rejected a live-safe real-time feed: %v", err)
	}
	if in2 == nil {
		t.Fatalf("NewLiveIngestor returned no ingestor for a live-safe feed")
	}

	// The general/backtest constructor STILL allows the backfill feed (backtests are
	// legitimate); enforcement is at the LIVE seam only, not blanket-blocked.
	if NewIngestor(k, backfill) == nil {
		t.Fatalf("NewIngestor must still build an ingestor for a backfill (backtest) source")
	}
	_ = context.Background()
}

// TestLiveSafe_Flags is a direct table over the LiveSafe() flags of the two stub
// sources, documenting the §4.6 rule at the type level.
func TestLiveSafe_Flags(t *testing.T) {
	if !NewRealtimeSource("rt", 1).LiveSafe() {
		t.Fatalf("realtime source must be live-safe")
	}
	if NewBackfillSource("bf", 1).LiveSafe() {
		t.Fatalf("backfill source must NOT be live-safe")
	}
}
