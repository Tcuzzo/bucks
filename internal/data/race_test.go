package data

import (
	"context"
	"errors"
	"testing"

	"bucks/internal/kernel"
	"bucks/internal/orders"
)

// TestIngest_ConcurrentProducerSingleOwner exercises the documented concurrency
// boundary: a PRODUCER goroutine writes frames onto the source channel while the
// Ingestor's single-owner Run loop consumes them on ANOTHER goroutine and drives
// the kernel. The channel is the only cross-goroutine handoff; the cache write and
// dispatch happen solely on the ingest goroutine. Run under `-race`, this proves
// there is no data race across the seam.
//
// Timestamps are strictly increasing so the kernel's monotonic logical clock is
// honored as frames arrive in order on the single channel.
func TestIngest_ConcurrentProducerSingleOwner(t *testing.T) {
	k := kernel.New()
	src := NewRealtimeSource("rt", 0) // unbuffered: forces real producer/consumer handoff
	in := NewIngestor(k, src)

	const n = 500
	var count int
	k.Bus().Subscribe("quote:RACE", func(b *kernel.Bus, e kernel.Event) {
		// Handler reads the cache during dispatch (single ingest goroutine drives
		// this via Submit) — no concurrent access with the producer goroutine.
		if _, ok := LatestQuote(k.Cache(), "RACE"); !ok {
			t.Errorf("cache empty during handler")
		}
		count++
	})

	// PRODUCER goroutine: writes frames then closes the stream. This is the only
	// other goroutine touching the source; it never touches the bus/cache.
	go func() {
		for i := 0; i < n; i++ {
			px, err := orders.NewDecimal(int64(100_000+i), 2) // 1000.00, 1000.01, ...
			if err != nil {
				t.Errorf("decimal: %v", err)
				return
			}
			src.Emit(Frame{Kind: FrameQuote, Quote: Quote{
				Symbol: "RACE",
				Bid:    px,
				Ask:    px,
				TS:     kernel.UnixNanos(i + 1),
			}})
		}
		src.CloseStream()
	}()

	// CONSUMER (single owner): the ingest loop on this goroutine.
	if err := in.Run(context.Background()); err != nil {
		t.Fatalf("ingestor run: %v", err)
	}

	if count != n {
		t.Fatalf("handler fired %d times, want %d", count, n)
	}
	// Final cached quote is the last produced one.
	last, ok := LatestQuote(k.Cache(), "RACE")
	if !ok {
		t.Fatalf("no final cached quote")
	}
	want, _ := orders.NewDecimal(int64(100_000+n-1), 2)
	if last.Bid.Cmp(want) != 0 {
		t.Fatalf("final cached bid = %s, want %s", last.Bid, want)
	}
}

// TestIngest_ContextCancel proves Run returns ctx.Err() when the context is
// canceled before the stream ends (the loop is the single owner and exits cleanly).
func TestIngest_ContextCancel(t *testing.T) {
	k := kernel.New()
	src := NewRealtimeSource("rt", 1)
	in := NewIngestor(k, src)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately; no frames emitted, stream left open

	err := in.Run(ctx)
	if err == nil {
		t.Fatalf("Run returned nil on canceled context, want ctx.Err()")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
}
