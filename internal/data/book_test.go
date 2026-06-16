package data

import (
	"errors"
	"testing"

	"bucks/internal/kernel"
	"bucks/internal/orders"
)

// fakeSnapshotter returns a fixed snapshot and counts how many times it was asked.
// It models the venue REST depth-snapshot call with NO network.
type fakeSnapshotter struct {
	snap  Snapshot
	calls int
}

func (f *fakeSnapshotter) fetch(symbol string) (Snapshot, error) {
	f.calls++
	s := f.snap
	s.Symbol = symbol
	return s, nil
}

// resyncRecorder subscribes to a symbol's resync topic and records every
// ResyncEvent so a test can assert it fired exactly once per gap.
func resyncRecorder(k *kernel.Kernel, symbol string) *[]ResyncEvent {
	var got []ResyncEvent
	k.Bus().Subscribe(ResyncTopic(symbol), func(b *kernel.Bus, e kernel.Event) {
		got = append(got, e.(ResyncEvent))
	})
	return &got
}

// TestBook_ContiguousDeltasApply proves a snapshot plus a contiguous run of
// absolute-level deltas builds the book correctly, including zero-qty removal.
func TestBook_ContiguousDeltasApply(t *testing.T) {
	b := NewSequencedBook("BTCUSDT")
	b.ApplySnapshot(Snapshot{
		Symbol:       "BTCUSDT",
		LastUpdateID: 100,
		Bids:         []Level{{Price: dec(t, "65000"), Qty: dec(t, "1.0")}},
		Asks:         []Level{{Price: dec(t, "65010"), Qty: dec(t, "2.0")}},
	})

	// Contiguous chain: pu must equal the prior u (snapshot lastUpdateId=100).
	deltas := []Delta{
		{Side: Bid, Price: dec(t, "64990"), Qty: dec(t, "3.0"), PU: 100, U: 101}, // add a deeper bid
		{Side: Ask, Price: dec(t, "65010"), Qty: dec(t, "0.5"), PU: 101, U: 102}, // update best ask qty
		{Side: Bid, Price: dec(t, "65000"), Qty: dec(t, "0"), PU: 102, U: 103},   // REMOVE the 65000 bid level
	}
	for i, d := range deltas {
		applied, err := b.ApplyDelta(d)
		if err != nil {
			t.Fatalf("delta %d unexpected gap: %v", i, err)
		}
		if !applied {
			t.Fatalf("delta %d not applied (stale?)", i)
		}
	}

	if b.LastUpdateID() != 103 {
		t.Fatalf("lastU = %d, want 103", b.LastUpdateID())
	}
	// 65000 bid was removed.
	if _, ok := b.QtyAt(Bid, dec(t, "65000")); ok {
		t.Fatalf("65000 bid level should have been removed by qty=0 delta")
	}
	// 64990 bid present at 3.0; best bid is now 64990.
	if q, ok := b.QtyAt(Bid, dec(t, "64990")); !ok || q.Cmp(dec(t, "3.0")) != 0 {
		t.Fatalf("64990 bid qty = %s ok=%v, want 3.0", q, ok)
	}
	bp, _, ok := b.BestBid()
	if !ok || bp.Cmp(dec(t, "64990")) != 0 {
		t.Fatalf("best bid = %s ok=%v, want 64990", bp, ok)
	}
	// best ask qty updated to 0.5.
	ap, aq, ok := b.BestAsk()
	if !ok || ap.Cmp(dec(t, "65010")) != 0 || aq.Cmp(dec(t, "0.5")) != 0 {
		t.Fatalf("best ask = %s qty %s ok=%v, want 65010 @ 0.5", ap, aq, ok)
	}
}

// TestGapRecovery_GapTriggersResync proves: a sequence GAP causes discard →
// snapshot refetch → replay → and emits ResyncEvent exactly once, with the book
// correct afterward.
func TestGapRecovery_GapTriggersResync(t *testing.T) {
	k := kernel.New()
	got := resyncRecorder(k, "BTCUSDT")

	// The fresh snapshot the recoverer will fetch on the gap.
	snapper := &fakeSnapshotter{snap: Snapshot{
		LastUpdateID: 200,
		Bids:         []Level{{Price: dec(t, "65000"), Qty: dec(t, "5.0")}},
		Asks:         []Level{{Price: dec(t, "65010"), Qty: dec(t, "5.0")}},
	}}

	book := NewSequencedBook("BTCUSDT")
	// Seed an initial state at lastU=100 so the first delta can apply contiguously.
	book.ApplySnapshot(Snapshot{LastUpdateID: 100,
		Bids: []Level{{Price: dec(t, "64000"), Qty: dec(t, "1.0")}}})

	g := NewGapRecoverer("BTCUSDT", book, snapper.fetch, k)

	// 1) Contiguous delta applies, no resync.
	if err := g.Apply(Delta{Side: Bid, Price: dec(t, "64000"), Qty: dec(t, "2.0"), PU: 100, U: 101}, 1); err != nil {
		t.Fatalf("contiguous apply: %v", err)
	}
	if len(*got) != 0 {
		t.Fatalf("resync fired on a contiguous delta")
	}

	// 2) GAP: pu=204 does not chain from lastU=101. Forces resync. This delta is
	//    genuinely non-contiguous AND non-straddling against the fresh snapshot
	//    (lastUpdateId=200): its pu=204 > 200, so it does NOT cover the snapshot
	//    boundary — it is a real post-snapshot gap and is DROPPED from replay (the
	//    next live delta drives forward). (A delta with pu <= 200 < u would STRADDLE
	//    the boundary and be applied — that case is covered by
	//    TestGapRecovery_StraddlingBufferedDeltaApplied.)
	if err := g.Apply(Delta{Side: Bid, Price: dec(t, "65000"), Qty: dec(t, "9.0"), PU: 204, U: 205}, 2); err != nil {
		t.Fatalf("gap apply: %v", err)
	}
	if snapper.calls != 1 {
		t.Fatalf("snapshot fetched %d times on gap, want 1", snapper.calls)
	}
	if len(*got) != 1 {
		t.Fatalf("ResyncEvent fired %d times on gap, want exactly 1", len(*got))
	}
	if (*got)[0].Reason != ResyncGap {
		t.Fatalf("resync reason = %v, want gap", (*got)[0].Reason)
	}
	// Book was rebuilt from the fresh snapshot (lastUpdateId=200).
	if book.LastUpdateID() != 200 {
		t.Fatalf("after resync lastU = %d, want 200 (snapshot)", book.LastUpdateID())
	}
	if q, ok := book.QtyAt(Bid, dec(t, "65000")); !ok || q.Cmp(dec(t, "5.0")) != 0 {
		t.Fatalf("post-resync 65000 bid = %s ok=%v, want 5.0 from snapshot", q, ok)
	}
	// The old 64000 level from the pre-gap book is gone (book was discarded).
	if _, ok := book.QtyAt(Bid, dec(t, "64000")); ok {
		t.Fatalf("pre-gap 64000 level survived the discard")
	}

	// 3) A contiguous delta AFTER the snapshot (pu=200) now applies and drives forward.
	if err := g.Apply(Delta{Side: Ask, Price: dec(t, "65010"), Qty: dec(t, "7.0"), PU: 200, U: 201}, 3); err != nil {
		t.Fatalf("post-resync contiguous apply: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("ResyncEvent fired again on a clean contiguous delta: %d total", len(*got))
	}
	if book.LastUpdateID() != 201 {
		t.Fatalf("lastU = %d after post-resync delta, want 201", book.LastUpdateID())
	}
}

// TestGapRecovery_BufferedDeltaMerge proves the standard snapshot/delta merge:
// buffered deltas with U <= snapshot lastUpdateId are DROPPED (already baked in),
// while contiguous ones after the snapshot are APPLIED, producing the correct book.
func TestGapRecovery_BufferedDeltaMerge(t *testing.T) {
	k := kernel.New()
	got := resyncRecorder(k, "ETHUSDT")

	// Snapshot lastUpdateId=300; the recoverer fetches this on the gap.
	snapper := &fakeSnapshotter{snap: Snapshot{
		LastUpdateID: 300,
		Bids:         []Level{{Price: dec(t, "3000"), Qty: dec(t, "10.0")}},
		Asks:         []Level{{Price: dec(t, "3001"), Qty: dec(t, "10.0")}},
	}}

	book := NewSequencedBook("ETHUSDT")
	book.ApplySnapshot(Snapshot{LastUpdateID: 50}) // arbitrary pre-gap state

	g := NewGapRecoverer("ETHUSDT", book, snapper.fetch, k)

	// First delta gaps immediately (pu=99 != lastU=50). On the resync the recoverer
	// replays its buffer against the fresh snapshot (lastUpdateId=300). This single
	// buffered delta has u=299 <= 300 -> it is STALE relative to the snapshot and
	// must be DROPPED (already baked into the snapshot).
	if err := g.Apply(Delta{Side: Bid, Price: dec(t, "3000"), Qty: dec(t, "999.0"), PU: 99, U: 299}, 1); err != nil {
		t.Fatalf("gap apply: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("resync count = %d, want 1", len(*got))
	}
	// The stale buffered delta (u=299 <= 300) must NOT have overwritten the snapshot
	// 3000 level (which is 10.0). If the merge wrongly applied it, qty would be 999.
	if q, ok := book.QtyAt(Bid, dec(t, "3000")); !ok || q.Cmp(dec(t, "10.0")) != 0 {
		t.Fatalf("stale buffered delta leaked: 3000 bid = %s, want 10.0 (snapshot)", q)
	}
	if book.LastUpdateID() != 300 {
		t.Fatalf("lastU = %d, want 300 (snapshot, stale delta dropped)", book.LastUpdateID())
	}

	// Now a delta that is contiguous AFTER the snapshot (pu=300, u=301) and one more
	// (pu=301,u=302) apply normally and update the book.
	if err := g.Apply(Delta{Side: Ask, Price: dec(t, "3001"), Qty: dec(t, "4.0"), PU: 300, U: 301}, 2); err != nil {
		t.Fatalf("post-snapshot contiguous apply: %v", err)
	}
	if err := g.Apply(Delta{Side: Bid, Price: dec(t, "2999"), Qty: dec(t, "8.0"), PU: 301, U: 302}, 3); err != nil {
		t.Fatalf("post-snapshot contiguous apply 2: %v", err)
	}
	if q, ok := book.QtyAt(Ask, dec(t, "3001")); !ok || q.Cmp(dec(t, "4.0")) != 0 {
		t.Fatalf("3001 ask = %s, want 4.0 after contiguous update", q)
	}
	if q, ok := book.QtyAt(Bid, dec(t, "2999")); !ok || q.Cmp(dec(t, "8.0")) != 0 {
		t.Fatalf("2999 bid = %s, want 8.0", q)
	}
	if len(*got) != 1 {
		t.Fatalf("resync fired again on contiguous deltas: %d total", len(*got))
	}
}

// TestGapRecovery_ReconnectResyncs proves a transport reconnect forces the same
// discard → snapshot → ResyncEvent path with reason=reconnect.
func TestGapRecovery_ReconnectResyncs(t *testing.T) {
	k := kernel.New()
	got := resyncRecorder(k, "BTCUSDT")

	snapper := &fakeSnapshotter{snap: Snapshot{
		LastUpdateID: 500,
		Bids:         []Level{{Price: dec(t, "65000"), Qty: dec(t, "1.0")}},
	}}
	book := NewSequencedBook("BTCUSDT")
	book.ApplySnapshot(Snapshot{LastUpdateID: 400,
		Bids: []Level{{Price: dec(t, "60000"), Qty: dec(t, "9.0")}}})

	g := NewGapRecoverer("BTCUSDT", book, snapper.fetch, k)

	if err := g.Reconnect(7); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if snapper.calls != 1 {
		t.Fatalf("snapshot fetched %d times on reconnect, want 1", snapper.calls)
	}
	if len(*got) != 1 {
		t.Fatalf("ResyncEvent fired %d times on reconnect, want exactly 1", len(*got))
	}
	if (*got)[0].Reason != ResyncReconnect {
		t.Fatalf("resync reason = %v, want reconnect", (*got)[0].Reason)
	}
	// Book rebuilt from the reconnect snapshot; old level gone.
	if book.LastUpdateID() != 500 {
		t.Fatalf("lastU = %d after reconnect, want 500", book.LastUpdateID())
	}
	if _, ok := book.QtyAt(Bid, dec(t, "60000")); ok {
		t.Fatalf("pre-reconnect 60000 level survived discard")
	}
	if q, ok := book.QtyAt(Bid, dec(t, "65000")); !ok || q.Cmp(dec(t, "1.0")) != 0 {
		t.Fatalf("post-reconnect 65000 bid = %s, want 1.0", q)
	}
}

// TestBook_StaleDeltaDroppedNotGap proves a delta whose entire range is already
// applied (u <= lastU) is dropped silently (applied=false, err=nil) — NOT reported
// as a gap. This is the normal duplicate/overlap case.
func TestBook_StaleDeltaDropped(t *testing.T) {
	b := NewSequencedBook("BTCUSDT")
	b.ApplySnapshot(Snapshot{LastUpdateID: 100,
		Bids: []Level{{Price: dec(t, "65000"), Qty: dec(t, "1.0")}}})

	applied, err := b.ApplyDelta(Delta{Side: Bid, Price: dec(t, "65000"), Qty: dec(t, "9.0"), PU: 90, U: 100})
	if err != nil {
		t.Fatalf("stale delta should not be a gap, got: %v", err)
	}
	if applied {
		t.Fatalf("stale delta (u=100 <= lastU=100) should NOT apply")
	}
	// Book unchanged.
	if q, _ := b.QtyAt(Bid, dec(t, "65000")); q.Cmp(dec(t, "1.0")) != 0 {
		t.Fatalf("stale delta mutated the book: 65000 = %s, want 1.0", q)
	}
}

// TestBook_StraddlingFirstDeltaApplied proves the Binance first-post-snapshot
// STRADDLE rule: the first diff event after a fresh snapshot does NOT chain exactly
// (its PU is at or below the snapshot lastUpdateId) but its U is past it — that event
// covers the snapshot boundary and MUST be applied, not reported as a gap. After it,
// strict contiguity is required again, and a genuinely non-contiguous forward delta
// still gaps. This test FAILS without the straddle rule (the book would return a
// GapError on the first event instead of applying it).
func TestBook_StraddlingFirstDeltaApplied(t *testing.T) {
	const L = 100 // snapshot lastUpdateId
	b := NewSequencedBook("BTCUSDT")
	b.ApplySnapshot(Snapshot{
		Symbol:       "BTCUSDT",
		LastUpdateID: L,
		Bids:         []Level{{Price: dec(t, "65000"), Qty: dec(t, "1.0")}},
		Asks:         []Level{{Price: dec(t, "65010"), Qty: dec(t, "2.0")}},
	})

	// STRADDLE: PU=95 < L=100 and U=105 > L. This is the normal first-post-snapshot
	// event on a real feed; it must APPLY (book updated, lastU advanced to U), NOT gap.
	straddle := Delta{Side: Bid, Price: dec(t, "64990"), Qty: dec(t, "4.0"), PU: 95, U: 105}
	applied, err := b.ApplyDelta(straddle)
	if err != nil {
		t.Fatalf("straddling first delta reported a GAP (must apply): %v", err)
	}
	if !applied {
		t.Fatalf("straddling first delta was not applied (treated as stale?)")
	}
	if b.LastUpdateID() != 105 {
		t.Fatalf("lastU = %d after straddle, want 105 (advanced to U)", b.LastUpdateID())
	}
	if q, ok := b.QtyAt(Bid, dec(t, "64990")); !ok || q.Cmp(dec(t, "4.0")) != 0 {
		t.Fatalf("straddle did not update the book: 64990 bid = %s ok=%v, want 4.0", q, ok)
	}

	// A FOLLOWING delta must now be STRICTLY contiguous (pu == lastU == 105) and apply.
	contiguous := Delta{Side: Ask, Price: dec(t, "65010"), Qty: dec(t, "0.5"), PU: 105, U: 106}
	applied, err = b.ApplyDelta(contiguous)
	if err != nil {
		t.Fatalf("contiguous delta after straddle gapped: %v", err)
	}
	if !applied || b.LastUpdateID() != 106 {
		t.Fatalf("contiguous delta after straddle: applied=%v lastU=%d, want applied lastU=106", applied, b.LastUpdateID())
	}

	// A NON-straddling, NON-contiguous forward delta (PU=200 > lastU=106) still GAPS.
	gap := Delta{Side: Bid, Price: dec(t, "64000"), Qty: dec(t, "9.0"), PU: 200, U: 205}
	applied, err = b.ApplyDelta(gap)
	if err == nil {
		t.Fatalf("a real forward gap (pu=200 > lastU=106) was NOT reported as a gap")
	}
	var ge *GapError
	if !errors.As(err, &ge) {
		t.Fatalf("gap error type = %T, want *GapError", err)
	}
	if applied {
		t.Fatalf("a gapping delta must not be applied")
	}
}

// TestGapRecovery_StraddlingBufferedDeltaApplied proves the buffered-delta merge
// handles a STRADDLING buffered delta during resync replay: a buffered delta whose
// U is past the fresh snapshot's lastUpdateId but whose PU is at/below it covers the
// snapshot boundary and must be APPLIED during replay (not dropped as stale, not
// rejected as a gap). This strengthens the merge beyond the purely-stale case and
// FAILS without the straddle rule (replay would gap and break, losing the level).
func TestGapRecovery_StraddlingBufferedDeltaApplied(t *testing.T) {
	k := kernel.New()
	got := resyncRecorder(k, "ETHUSDT")

	// Snapshot lastUpdateId=300; the recoverer fetches this on the gap.
	snapper := &fakeSnapshotter{snap: Snapshot{
		LastUpdateID: 300,
		Bids:         []Level{{Price: dec(t, "3000"), Qty: dec(t, "10.0")}},
		Asks:         []Level{{Price: dec(t, "3001"), Qty: dec(t, "10.0")}},
	}}

	book := NewSequencedBook("ETHUSDT")
	book.ApplySnapshot(Snapshot{LastUpdateID: 50}) // arbitrary pre-gap state

	g := NewGapRecoverer("ETHUSDT", book, snapper.fetch, k)

	// This first delta gaps (pu=99 != lastU=50), forcing a resync. It is also the
	// STRADDLE for the fresh snapshot: PU=290 <= 300 and U=305 > 300. During replay
	// against the snapshot (lastUpdateId=300) it must be APPLIED (boundary-covering),
	// updating the 3000 bid to 77.0 and advancing lastU to 305 — NOT dropped/gapped.
	straddle := Delta{Side: Bid, Price: dec(t, "3000"), Qty: dec(t, "77.0"), PU: 290, U: 305}
	if err := g.Apply(straddle, 1); err != nil {
		t.Fatalf("gap/resync apply: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("resync count = %d, want 1", len(*got))
	}
	if q, ok := book.QtyAt(Bid, dec(t, "3000")); !ok || q.Cmp(dec(t, "77.0")) != 0 {
		t.Fatalf("straddling buffered delta not applied on replay: 3000 bid = %s ok=%v, want 77.0", q, ok)
	}
	if book.LastUpdateID() != 305 {
		t.Fatalf("lastU = %d after straddle replay, want 305", book.LastUpdateID())
	}

	// A following STRICTLY contiguous live delta (pu=305) applies and drives forward.
	if err := g.Apply(Delta{Side: Ask, Price: dec(t, "3001"), Qty: dec(t, "4.0"), PU: 305, U: 306}, 2); err != nil {
		t.Fatalf("post-straddle contiguous apply: %v", err)
	}
	if q, ok := book.QtyAt(Ask, dec(t, "3001")); !ok || q.Cmp(dec(t, "4.0")) != 0 {
		t.Fatalf("3001 ask = %s ok=%v, want 4.0 after contiguous update", q, ok)
	}
	if book.LastUpdateID() != 306 {
		t.Fatalf("lastU = %d, want 306", book.LastUpdateID())
	}
	if len(*got) != 1 {
		t.Fatalf("resync fired again on a contiguous delta: %d total", len(*got))
	}
}

// TestBook_DecimalExactInBook proves a level qty that would drift under float64 is
// stored and returned EXACTLY by the book.
func TestBook_DecimalExactInBook(t *testing.T) {
	b := NewSequencedBook("BTCUSDT")
	b.ApplySnapshot(Snapshot{LastUpdateID: 1})
	// 0.1 + 0.2 set across two deltas then summed must equal exactly 0.3.
	q1 := dec(t, "0.1")
	q2 := dec(t, "0.2")
	sum, err := q1.Add(q2)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, e := b.ApplyDelta(Delta{Side: Bid, Price: dec(t, "100"), Qty: sum, PU: 1, U: 2}); e != nil {
		t.Fatalf("apply: %v", e)
	}
	got, ok := b.QtyAt(Bid, dec(t, "100"))
	if !ok {
		t.Fatalf("level missing")
	}
	if got.String() != "0.3" {
		t.Fatalf("book stored qty %q, want exactly \"0.3\" (float64 would give 0.30000000000000004)", got.String())
	}
	if got.Cmp(dec(t, "0.3")) != 0 {
		t.Fatalf("book qty %s != 0.3", got)
	}
	_ = orders.ZeroDecimal
}
