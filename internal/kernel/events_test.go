package kernel

import "bucks/internal/orders"

// quoteEvent is a test event carrying a price in orders.Decimal (NEVER float64),
// proving the kernel moves money-bearing events while honoring the no-float rule.
type quoteEvent struct {
	ts     UnixNanos
	symbol string
	px     orders.Decimal
}

func (q quoteEvent) Topic() string        { return "quote" }
func (q quoteEvent) Timestamp() UnixNanos { return q.ts }

// signalEvent is a non-money control event used to exercise routing, cascades,
// and ordering.
type signalEvent struct {
	ts   UnixNanos
	kind string
}

func (s signalEvent) Topic() string        { return "signal" }
func (s signalEvent) Timestamp() UnixNanos { return s.ts }

// cascadeEvent is published BY a handler to prove cascade ordering (it must be
// processed after the event that triggered it, not re-entrantly).
type cascadeEvent struct {
	ts  UnixNanos
	tag string
}

func (c cascadeEvent) Topic() string        { return "cascade" }
func (c cascadeEvent) Timestamp() UnixNanos { return c.ts }

// mustDec parses a decimal literal for tests, panicking on a bad literal (test
// input is always a known-good constant).
func mustDec(s string) orders.Decimal { return orders.MustParseDecimal(s) }
