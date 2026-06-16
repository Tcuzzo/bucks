package kernel

import (
	"bucks/internal/orders"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
)

// buildScenario wires a fresh kernel with a small but realistic handler graph and
// returns it together with an *applyLog pointer the handlers append to. The graph:
//
//   - "quote" handler: writes the latest price per symbol to the cache (write-
//     then-publish), records the application, and on a round-number price publishes
//     a "signal" cascade event.
//   - "signal" handler: writes a per-symbol signal count to the cache and records.
//
// The resulting final state is (cache contents + the ordered application log).
// Fingerprinting that state and comparing across two fresh kernels fed the SAME
// events is the replay-equality gate.
func buildScenario() (*Kernel, *[]string) {
	k := New()
	cache := k.Cache()
	log := new([]string)

	k.Bus().Subscribe("quote", func(bus *Bus, e Event) {
		q := e.(quoteEvent)
		// write-then-publish: cache first, then any cascade.
		cache.Put("quote", q.symbol, q.px)
		*log = append(*log, fmt.Sprintf("@%s quote %s=%s", k.Now(), q.symbol, q.px.String()))

		// On a price that is an exact multiple of 100, emit a signal cascade.
		hundred := mustDec("100")
		rem, err := q.px.Quo(hundred)
		if err == nil && rem.Cmp(rem.Trunc(0)) == 0 {
			bus.Publish(signalEvent{ts: q.ts, kind: q.symbol})
		}
	})

	k.Bus().Subscribe("signal", func(_ *Bus, e Event) {
		sym := e.(signalEvent).kind
		var n int
		if v, ok := cache.Get("signalcount", sym); ok {
			n = v.(int)
		}
		n++
		cache.Put("signalcount", sym, n)
		*log = append(*log, fmt.Sprintf("@%s signal %s #%d", k.Now(), sym, n))
	})

	return k, log
}

// fingerprint hashes the kernel's final state: every cache entry (in sorted key
// order, so map iteration cannot perturb it) plus the ordered application log.
// Identical state => identical hex digest.
func fingerprint(k *Kernel, log []string) string {
	h := sha256.New()
	cache := k.Cache()
	for _, kk := range cache.Keys() { // Keys() is sorted -> deterministic
		v, _ := cache.Get(kk[0], kk[1])
		// Render the value in a stable textual form. Decimal has a canonical
		// String(); ints format directly.
		var vs string
		switch tv := v.(type) {
		case orders.Decimal:
			vs = tv.String()
		default:
			vs = fmt.Sprintf("%v", tv)
		}
		fmt.Fprintf(h, "C|%s|%s|%s\n", kk[0], kk[1], vs)
	}
	for i, line := range log {
		fmt.Fprintf(h, "L|%d|%s\n", i, line)
	}
	// Include the final logical clock so a time difference would change the hash.
	fmt.Fprintf(h, "T|%s\n", k.Now())
	return hex.EncodeToString(h.Sum(nil))
}

// mixedEvents returns a fixed, ordered sequence of >=10 mixed events (quotes at
// round and non-round prices across two symbols, interleaved). Round-number quotes
// trigger signal cascades, exercising the cascade path in the replay.
func mixedEvents() []Event {
	return []Event{
		quoteEvent{ts: 1, symbol: "BTC", px: mustDec("100")},     // round -> cascade
		quoteEvent{ts: 2, symbol: "ETH", px: mustDec("2000")},    // round -> cascade
		quoteEvent{ts: 3, symbol: "BTC", px: mustDec("100.50")},  // not round
		quoteEvent{ts: 4, symbol: "ETH", px: mustDec("2001.25")}, // not round
		quoteEvent{ts: 5, symbol: "BTC", px: mustDec("200")},     // round -> cascade
		quoteEvent{ts: 6, symbol: "BTC", px: mustDec("250.75")},  // not round
		quoteEvent{ts: 7, symbol: "ETH", px: mustDec("3000")},    // round -> cascade
		quoteEvent{ts: 8, symbol: "BTC", px: mustDec("300")},     // round -> cascade
		quoteEvent{ts: 9, symbol: "ETH", px: mustDec("3100.10")}, // not round
		quoteEvent{ts: 10, symbol: "BTC", px: mustDec("400")},    // round -> cascade
		quoteEvent{ts: 11, symbol: "ETH", px: mustDec("4000")},   // round -> cascade
		quoteEvent{ts: 12, symbol: "BTC", px: mustDec("404.04")}, // not round
	}
}

// TestReplayEqualitySameEventsIdenticalState is the HEADLINE gate. It feeds an
// IDENTICAL ordered event sequence through two FRESH kernels and asserts the
// final-state fingerprint (cache contents + application log + logical clock hash)
// is BIT-IDENTICAL across the two runs. Without this, the slice fails.
func TestReplayEqualitySameEventsIdenticalState(t *testing.T) {
	k1, log1 := buildScenario()
	k1.Run(mixedEvents())
	fp1 := fingerprint(k1, *log1)

	k2, log2 := buildScenario()
	k2.Run(mixedEvents())
	fp2 := fingerprint(k2, *log2)

	if fp1 != fp2 {
		t.Fatalf("replay NOT deterministic:\n run1=%s\n run2=%s", fp1, fp2)
	}
	// Sanity: the fingerprint must reflect non-empty state (guards a vacuous pass).
	if k1.Cache().Len() == 0 || len(*log1) == 0 {
		t.Fatalf("scenario produced empty state (cache=%d log=%d) — fingerprint vacuous",
			k1.Cache().Len(), len(*log1))
	}
	t.Logf("replay-equality fingerprint = %s (cache entries=%d, log lines=%d)",
		fp1, k1.Cache().Len(), len(*log1))
}

// TestReplayEqualityBacktestClockMatchesDirectRun asserts the SAME events driven
// through the BacktestClock yield the SAME fingerprint as a direct kernel run —
// the backtest path is just a clock over the identical engine, so it must be
// bit-identical too.
func TestReplayEqualityBacktestClockMatchesDirectRun(t *testing.T) {
	kDirect, logDirect := buildScenario()
	kDirect.Run(mixedEvents())
	fpDirect := fingerprint(kDirect, *logDirect)

	kBack, logBack := buildScenario()
	NewBacktestClock(mixedEvents()).Drive(kBack)
	fpBack := fingerprint(kBack, *logBack)

	if fpDirect != fpBack {
		t.Fatalf("backtest clock diverged from direct run:\n direct=%s\n backtest=%s",
			fpDirect, fpBack)
	}
}

// TestReplayEqualityLiveClockMatchesBacktest asserts the LiveClock (events fed as
// they "arrive") reaches the SAME fingerprint as the BacktestClock over the same
// ordered events — proving paper (backtest) and live cannot diverge on identical
// input, the whole point of one-engine-two-clocks.
func TestReplayEqualityLiveClockMatchesBacktest(t *testing.T) {
	kBack, logBack := buildScenario()
	NewBacktestClock(mixedEvents()).Drive(kBack)
	fpBack := fingerprint(kBack, *logBack)

	kLive, logLive := buildScenario()
	live := NewLiveClock()
	for _, e := range mixedEvents() {
		live.Feed(e)
	}
	live.Drive(kLive)
	fpLive := fingerprint(kLive, *logLive)

	if fpBack != fpLive {
		t.Fatalf("live clock diverged from backtest:\n backtest=%s\n live=%s", fpBack, fpLive)
	}
}

// TestDispatchOrderStableAcrossSubscriptionOrder asserts the kernel's result is
// stable regardless of the ORDER handlers are registered, for INDEPENDENT handlers
// (handlers whose effects don't depend on each other). Two kernels register the
// same two independent quote handlers in opposite order; the resulting cache state
// is identical. (Registration order only matters for ordering OBSERVABLE side
// effects of dependent handlers — which the cascade tests cover; independent
// handlers commute, and we prove the engine doesn't inject nondeterminism.)
func TestDispatchOrderStableAcrossSubscriptionOrder(t *testing.T) {
	makeKernel := func(reverse bool) string {
		k := New()
		cache := k.Cache()
		hi := func(_ *Bus, e Event) { cache.Put("hi", e.(quoteEvent).symbol, e.(quoteEvent).px) }
		lo := func(_ *Bus, e Event) {
			// independent: writes a different cache namespace
			cache.Put("lo", e.(quoteEvent).symbol, e.(quoteEvent).px)
		}
		if reverse {
			k.Bus().Subscribe("quote", lo)
			k.Bus().Subscribe("quote", hi)
		} else {
			k.Bus().Subscribe("quote", hi)
			k.Bus().Subscribe("quote", lo)
		}
		k.Run([]Event{
			quoteEvent{ts: 1, symbol: "BTC", px: mustDec("100")},
			quoteEvent{ts: 2, symbol: "ETH", px: mustDec("200")},
		})
		// fingerprint just the cache (no log here)
		return fingerprint(k, nil)
	}

	a := makeKernel(false)
	b := makeKernel(true)
	if a != b {
		t.Fatalf("independent-handler result depended on subscription order:\n forward=%s\n reverse=%s", a, b)
	}
}
