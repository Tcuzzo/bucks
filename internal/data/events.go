// Package data is BUCKS's market-data ingest spine: it turns a venue feed into
// kernel Events that flow through the bus, keeps the latest quote per symbol in
// the kernel cache, recovers an order book across WebSocket sequence GAPs (the
// headline reliability logic), and enforces the live-safe rule that delayed /
// backfill feeds (yfinance / Alpha Vantage) are BANNED from the live trigger path
// (build spec §4.6).
//
// Design rules carried from the kernel (see internal/kernel):
//   - Prices and sizes are orders.Decimal — NEVER float64. Binary floats cannot
//     represent most decimal money values exactly and drift across operations.
//   - Market-data events carry the VENUE timestamp as their logical event-time
//     (Timestamp()), so the kernel's logical clock is driven by event-time, never
//     the wall clock.
//   - Cache writes use write-then-publish: the Ingestor Put()s the latest quote
//     BEFORE Publish()ing its event, so any handler woken by the event already
//     sees the fresh cache value (kernel.Cache invariant).
package data

import (
	"context"
	"fmt"

	"bucks/internal/kernel"
	"bucks/internal/orders"
)

// QuoteKind is the kernel.Cache "kind" under which the latest Quote per symbol is
// stored. Handlers read the freshest quote via cache.Get(QuoteKind, symbol).
const QuoteKind = "quote"

// Quote is a top-of-book market-data event: bid/ask (and an optional last trade
// price) for a symbol at a venue timestamp. It implements kernel.Event so it can
// flow through the bus and drive the logical clock. All prices are orders.Decimal.
type Quote struct {
	Symbol string
	Bid    orders.Decimal
	Ask    orders.Decimal
	Last   orders.Decimal
	TS     kernel.UnixNanos // venue event-time
}

// Topic routes per symbol, e.g. "quote:AAPL", so a handler can subscribe to a
// single instrument's quotes without seeing every other symbol.
func (q Quote) Topic() string { return "quote:" + q.Symbol }

// Timestamp is the venue event-time; the kernel advances Now() to this on dispatch.
func (q Quote) Timestamp() kernel.UnixNanos { return q.TS }

// Trade is a single executed-trade print: a price and size at a venue timestamp.
type Trade struct {
	Symbol string
	Price  orders.Decimal
	Size   orders.Decimal
	TS     kernel.UnixNanos
}

// Topic routes per symbol, e.g. "trade:AAPL".
func (t Trade) Topic() string { return "trade:" + t.Symbol }

// Timestamp is the venue event-time of the print.
func (t Trade) Timestamp() kernel.UnixNanos { return t.TS }

// Bar is an OHLCV aggregate over a fixed interval. Open/High/Low/Close and Volume
// are exact decimal. The bar's timestamp is its close time (the venue's stamp).
type Bar struct {
	Symbol string
	Open   orders.Decimal
	High   orders.Decimal
	Low    orders.Decimal
	Close  orders.Decimal
	Volume orders.Decimal
	TS     kernel.UnixNanos
}

// Topic routes per symbol, e.g. "bar:AAPL".
func (b Bar) Topic() string { return "bar:" + b.Symbol }

// Timestamp is the venue event-time (bar close) of the aggregate.
func (b Bar) Timestamp() kernel.UnixNanos { return b.TS }

// Frame is one decoded message from a DataSource. Exactly one of Quote/Trade/Bar
// is the payload, selected by Kind. A source decodes its wire format into Frames;
// the Ingestor turns Frames into kernel Events. (Order-book delta frames used by
// the gap recoverer are a separate type — see Delta in book.go — because they are
// not published as bus events directly; they mutate the SequencedBook.)
type Frame struct {
	Kind  FrameKind
	Quote Quote
	Trade Trade
	Bar   Bar
}

// FrameKind tags which payload a Frame carries.
type FrameKind int

const (
	// FrameQuote carries a Quote payload.
	FrameQuote FrameKind = iota
	// FrameTrade carries a Trade payload.
	FrameTrade
	// FrameBar carries a Bar payload.
	FrameBar
)

// DataSource is a market-data feed. A source is subscribed to a set of symbols,
// then streams decoded Frames on Frames() until Close() is called or the stream
// ends (the channel is closed). LiveSafe reports whether this feed is permitted on
// the LIVE TRADE path: real-time exchange feeds are live-safe; delayed/backfill
// feeds (yfinance / Alpha Vantage / free-Polygon) are NOT and must be refused on
// the live trigger path (see AssertLiveTradePath in livesafe.go).
//
// Concurrency: a source's Frames() channel is written by the source's own
// producer (e.g. a network read loop). It is consumed by exactly one Ingestor
// goroutine — the single-owner reader. The channel is the only cross-goroutine
// handoff between the source and the ingest pipeline.
type DataSource interface {
	// Subscribe registers interest in symbols and begins streaming. It returns an
	// error if the subscription cannot be established.
	Subscribe(ctx context.Context, symbols []string) error
	// Frames returns the receive-only stream of decoded frames. The source closes
	// the channel when the feed ends or Close is called.
	Frames() <-chan Frame
	// LiveSafe reports whether this source may feed the live trade path.
	LiveSafe() bool
	// Close stops the source and releases its resources. It is idempotent.
	Close() error
}

// Ingestor reads a DataSource's frames on a SINGLE owner goroutine and feeds them
// through the kernel as events (the live entry point, kernel.Submit), updating the
// kernel.Cache with the latest quote per symbol using write-then-publish.
//
// CONCURRENCY BOUNDARY (documented per the slice contract):
//
//	The Ingestor's Run loop is the SINGLE owner of the source's Frames() channel —
//	exactly one goroutine ever receives from it. For each quote frame it writes the
//	latest quote into the cache and THEN submits the event to the kernel; Submit
//	enqueues on the bus and drains it single-threaded. The kernel.Submit call is the
//	ONLY cross-goroutine handoff out of the ingest goroutine into the deterministic
//	engine: the source produces frames on ITS goroutine, the Ingestor (one
//	goroutine) consumes the channel and drives Submit, and dispatch inside Submit is
//	strictly single-threaded. No handler ever runs concurrently with the cache
//	write, because the write happens (cache.Put) before Submit and Submit's drain is
//	single-threaded. This preserves the kernel's single-threaded-dispatch invariant
//	even though the source produces on its own goroutine.
type Ingestor struct {
	k   *kernel.Kernel
	src DataSource
}

// NewIngestor wires an Ingestor to feed src's frames through k. k.Cache() receives
// the latest quote per symbol; k.Submit drives dispatch.
//
// GENERAL / BACKTEST constructor: this does NOT enforce live-safety, because
// backfill ingestion (yfinance / Alpha Vantage) is LEGITIMATE for backtests. The
// LIVE TRADE path must NOT use this — it must construct via NewLiveIngestor, which
// refuses a non-live-safe source. (Trading on delayed data is the #1 silent killer;
// the enforcement lives at the live construction seam, not here.)
func NewIngestor(k *kernel.Kernel, src DataSource) *Ingestor {
	return &Ingestor{k: k, src: src}
}

// NewLiveIngestor is the LIVE TRADE path constructor. It is the production seam that
// enforces build spec §4.6: it passes src through AssertLiveTradePath and returns
// the *ErrNotLiveSafe (naming the source) if src is a delayed/backfill feed, so a
// backfill source can never be silently wired onto the live trigger path. On a
// live-safe source it returns a ready Ingestor and a nil error.
//
// The LIVE trade path MUST construct via NewLiveIngestor (never NewIngestor) so the
// live-safe gate fires at the construction boundary. Backtests use NewIngestor.
func NewLiveIngestor(k *kernel.Kernel, src DataSource) (*Ingestor, error) {
	if err := AssertLiveTradePath(src); err != nil {
		return nil, err
	}
	return NewIngestor(k, src), nil
}

// Run consumes the source's frames until the channel is closed or ctx is done,
// submitting each as a kernel event (and, for quotes, updating the cache
// write-then-publish before the submit drains). It returns nil on a clean drain
// (channel closed) and ctx.Err() if the context was canceled. Run is the
// single-owner reader of the source stream: do not call it from more than one
// goroutine. This is the live feed loop: a LiveClock source calls Submit per event.
func (in *Ingestor) Run(ctx context.Context) error {
	frames := in.src.Frames()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f, ok := <-frames:
			if !ok {
				return nil
			}
			in.submit(f)
		}
	}
}

// submit maps one frame to its kernel event and drives it through the kernel. For a
// quote it uses write-then-publish (cache.Put BEFORE the submit that drains), so a
// handler woken by the event already sees the fresh cache value; trades and bars
// are submitted directly (no per-symbol latest-cache for them in v1).
func (in *Ingestor) submit(f Frame) {
	switch f.Kind {
	case FrameQuote:
		// Write-then-publish: latest quote into the cache FIRST, then Submit (which
		// enqueues and drains single-threaded). A handler that runs in response to
		// the quote therefore reads the already-updated cache value.
		in.k.Cache().Put(QuoteKind, f.Quote.Symbol, f.Quote)
		in.k.Submit(f.Quote)
	case FrameTrade:
		in.k.Submit(f.Trade)
	case FrameBar:
		in.k.Submit(f.Bar)
	default:
		// An unknown frame kind is a programming error in a source decoder; surface
		// it loudly rather than silently dropping market data.
		panic(fmt.Sprintf("data: unknown frame kind %d", f.Kind))
	}
}

// LatestQuote reads the freshest cached quote for symbol, or (Quote{}, false) if
// none has been ingested yet. It is a typed convenience over the kernel cache.
func LatestQuote(cache *kernel.Cache, symbol string) (Quote, bool) {
	v, ok := cache.Get(QuoteKind, symbol)
	if !ok {
		return Quote{}, false
	}
	q, ok := v.(Quote)
	return q, ok
}
