package data

import (
	"context"
	"testing"

	"bucks/internal/kernel"
	"bucks/internal/orders"
)

func dec(t *testing.T, s string) orders.Decimal {
	t.Helper()
	d, err := orders.ParseDecimal(s)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", s, err)
	}
	return d
}

// runIngest runs the ingestor to completion. The fake stream is finite and closed,
// so Run returns after the single-owner read loop sees the closed channel. Each
// frame is driven through the kernel via Submit inside Run, so handlers have
// already fired by the time Run returns — dispatch is single-threaded and inline.
func runIngest(t *testing.T, in *Ingestor) {
	t.Helper()
	if err := in.Run(context.Background()); err != nil {
		t.Fatalf("ingestor run: %v", err)
	}
}

// TestIngest_QuotesLandOnBusInOrder proves a fake source emitting quotes results in
// quote events on the bus, recorded by a subscriber IN ORDER.
func TestIngest_QuotesLandOnBusInOrder(t *testing.T) {
	k := kernel.New()
	src := NewRealtimeSource("rt", 8)
	in := NewIngestor(k, src)

	var got []Quote
	k.Bus().Subscribe("quote:AAPL", func(b *kernel.Bus, e kernel.Event) {
		got = append(got, e.(Quote))
	})

	quotes := []Quote{
		{Symbol: "AAPL", Bid: dec(t, "100.10"), Ask: dec(t, "100.20"), TS: 1},
		{Symbol: "AAPL", Bid: dec(t, "100.11"), Ask: dec(t, "100.21"), TS: 2},
		{Symbol: "AAPL", Bid: dec(t, "100.12"), Ask: dec(t, "100.22"), TS: 3},
	}
	for _, q := range quotes {
		src.Emit(Frame{Kind: FrameQuote, Quote: q})
	}
	src.CloseStream()

	runIngest(t, in)

	if len(got) != len(quotes) {
		t.Fatalf("want %d quote events, got %d", len(quotes), len(got))
	}
	for i := range quotes {
		if got[i].TS != quotes[i].TS {
			t.Fatalf("event %d out of order: want ts=%d got ts=%d", i, quotes[i].TS, got[i].TS)
		}
		if got[i].Bid.Cmp(quotes[i].Bid) != 0 {
			t.Fatalf("event %d bid mismatch: want %s got %s", i, quotes[i].Bid, got[i].Bid)
		}
	}
}

// TestIngest_WriteThenPublish proves the cache holds the latest quote and that a
// handler woken BY the quote event already sees the freshest cache value (the
// write-then-publish invariant): the handler reads cache and must observe the same
// quote that triggered it, not an older one.
func TestIngest_WriteThenPublish(t *testing.T) {
	k := kernel.New()
	src := NewRealtimeSource("rt", 8)
	in := NewIngestor(k, src)

	var seenInHandler []orders.Decimal
	k.Bus().Subscribe("quote:MSFT", func(b *kernel.Bus, e kernel.Event) {
		ev := e.(Quote)
		// Read the cache DURING dispatch of this event. Write-then-publish
		// guarantees the cache already holds THIS event's value.
		cached, ok := LatestQuote(k.Cache(), "MSFT")
		if !ok {
			t.Errorf("cache had no quote for MSFT during handler for ts=%d", ev.TS)
			return
		}
		if cached.Bid.Cmp(ev.Bid) != 0 {
			t.Errorf("write-then-publish violated at ts=%d: event bid=%s but cache bid=%s",
				ev.TS, ev.Bid, cached.Bid)
		}
		seenInHandler = append(seenInHandler, cached.Bid)
	})

	src.Emit(Frame{Kind: FrameQuote, Quote: Quote{Symbol: "MSFT", Bid: dec(t, "300.01"), Ask: dec(t, "300.05"), TS: 10}})
	src.Emit(Frame{Kind: FrameQuote, Quote: Quote{Symbol: "MSFT", Bid: dec(t, "300.02"), Ask: dec(t, "300.06"), TS: 11}})
	src.CloseStream()

	runIngest(t, in)

	if len(seenInHandler) != 2 {
		t.Fatalf("handler fired %d times, want 2", len(seenInHandler))
	}
	// Cache must end on the LATEST quote.
	latest, ok := LatestQuote(k.Cache(), "MSFT")
	if !ok {
		t.Fatalf("no cached quote after ingest")
	}
	if latest.Bid.Cmp(dec(t, "300.02")) != 0 {
		t.Fatalf("cache latest bid = %s, want 300.02", latest.Bid)
	}
}

// TestIngest_TradesAndBars proves non-quote frames are published as their event
// types on the correct topics.
func TestIngest_TradesAndBars(t *testing.T) {
	k := kernel.New()
	src := NewRealtimeSource("rt", 8)
	in := NewIngestor(k, src)

	var trades []Trade
	var bars []Bar
	k.Bus().Subscribe("trade:BTCUSDT", func(b *kernel.Bus, e kernel.Event) { trades = append(trades, e.(Trade)) })
	k.Bus().Subscribe("bar:BTCUSDT", func(b *kernel.Bus, e kernel.Event) { bars = append(bars, e.(Bar)) })

	src.Emit(Frame{Kind: FrameTrade, Trade: Trade{Symbol: "BTCUSDT", Price: dec(t, "65000.50"), Size: dec(t, "0.01"), TS: 1}})
	src.Emit(Frame{Kind: FrameBar, Bar: Bar{Symbol: "BTCUSDT", Open: dec(t, "64900"), High: dec(t, "65100"), Low: dec(t, "64800"), Close: dec(t, "65000.50"), Volume: dec(t, "12.5"), TS: 2}})
	src.CloseStream()

	runIngest(t, in)

	if len(trades) != 1 || trades[0].Price.Cmp(dec(t, "65000.50")) != 0 {
		t.Fatalf("trade not published correctly: %+v", trades)
	}
	if len(bars) != 1 || bars[0].Close.Cmp(dec(t, "65000.50")) != 0 {
		t.Fatalf("bar not published correctly: %+v", bars)
	}
}

// TestIngest_DecimalExactThroughPipeline proves a price that drifts under float64
// passes through the ingest pipeline EXACTLY (no float round-trip). 0.1+0.2 is the
// canonical float drift case; we feed 0.30000000000000004-prone values and assert
// exact equality of the cached and emitted decimal.
func TestIngest_DecimalExactThroughPipeline(t *testing.T) {
	k := kernel.New()
	src := NewRealtimeSource("rt", 4)
	in := NewIngestor(k, src)

	// 0.1 and 0.2 are not exactly representable in float64; their sum is 0.30000000000000004.
	// As decimals through BUCKS they must remain exactly 0.1, 0.2, and the cached
	// value must equal the literal we put in.
	bid := dec(t, "0.1")
	ask := dec(t, "0.2")

	var emitted Quote
	k.Bus().Subscribe("quote:DRIFT", func(b *kernel.Bus, e kernel.Event) { emitted = e.(Quote) })

	src.Emit(Frame{Kind: FrameQuote, Quote: Quote{Symbol: "DRIFT", Bid: bid, Ask: ask, TS: 1}})
	src.CloseStream()
	runIngest(t, in)

	if emitted.Bid.Cmp(dec(t, "0.1")) != 0 {
		t.Fatalf("bid drifted through pipeline: got %s want 0.1", emitted.Bid)
	}
	if emitted.Ask.Cmp(dec(t, "0.2")) != 0 {
		t.Fatalf("ask drifted through pipeline: got %s want 0.2", emitted.Ask)
	}
	// Exact decimal addition stays exact (0.3, not 0.30000000000000004).
	sum, err := emitted.Bid.Add(emitted.Ask)
	if err != nil {
		t.Fatalf("decimal add: %v", err)
	}
	if sum.Cmp(dec(t, "0.3")) != 0 {
		t.Fatalf("decimal sum drifted: got %s want exactly 0.3", sum)
	}
	// String form must be exactly "0.3" — proof there was no binary-float round-trip.
	if sum.String() != "0.3" {
		t.Fatalf("decimal sum string = %q, want \"0.3\" (float64 would give 0.30000000000000004)", sum.String())
	}
}
