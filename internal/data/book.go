package data

import (
	"fmt"
	"sort"

	"bucks/internal/kernel"
	"bucks/internal/orders"
)

// Side is a book side: bid or ask.
type Side int

const (
	// Bid is the buy side of the book.
	Bid Side = iota
	// Ask is the sell side of the book.
	Ask
)

// String renders the side.
func (s Side) String() string {
	if s == Bid {
		return "bid"
	}
	return "ask"
}

// Delta is one ABSOLUTE order-book level update, Binance-diff-stream style. Qty is
// the NEW absolute quantity resting at Price on Side — NOT a delta of quantity.
//
// ABSOLUTE-LEVEL SEMANTICS (documented per the slice contract): each delta sets
// the resting quantity at a price level to Qty. A Qty of ZERO REMOVES the level
// (Binance: "If the quantity is 0, remove the price level"). This is why the book
// can be reconstructed from a snapshot plus a contiguous run of deltas: every
// delta is self-describing, not relative.
//
// Sequencing (Binance depth-stream model): each delta carries a pair
//   - PU (prevUpdateID): the update ID immediately PRECEDING this delta's batch.
//   - U  (updateID):     this delta's update ID.
//
// A stream is CONTIGUOUS iff each new delta's PU equals the last applied delta's
// U. A break (PU != lastU) is a GAP and means at least one update was missed.
type Delta struct {
	Side  Side
	Price orders.Decimal
	Qty   orders.Decimal // absolute resting qty; 0 == remove the level
	PU    int64          // prevUpdateID
	U     int64          // updateID
}

// level pairs a price with its resting quantity. Stored in a sorted slice per side
// so the book is deterministic to iterate (no map-order dependence).
type level struct {
	price orders.Decimal
	qty   orders.Decimal
}

// SequencedBook is a single-symbol order book maintained from a snapshot plus a
// contiguous stream of ABSOLUTE-level deltas. It tracks lastU (the update ID of
// the last applied delta); a delta is applied if it is contiguous (delta.PU ==
// lastU) OR it is the first-post-snapshot STRADDLE event (delta.PU <= lastU <
// delta.U), which covers the snapshot boundary. Any other forward delta (PU >
// lastU) is a GAP and is reported so the caller (GapRecoverer) can resync. The
// book holds no float64: all prices and quantities are orders.Decimal.
//
// This is NOT goroutine-safe; it is owned by a single ingest/recovery goroutine
// (the same single-owner boundary as the Ingestor).
type SequencedBook struct {
	symbol string
	bids   []level // sorted by price (ascending); best bid is the last element
	asks   []level // sorted by price (ascending); best ask is the first element
	lastU  int64   // updateID of the last applied delta (snapshot lastUpdateId at reset)
	ready  bool    // false until a snapshot has been applied
}

// NewSequencedBook returns an empty, not-ready book for symbol. It must be seeded
// with a snapshot (ApplySnapshot) before deltas can be applied contiguously.
func NewSequencedBook(symbol string) *SequencedBook {
	return &SequencedBook{symbol: symbol}
}

// Snapshot is a full REST order-book snapshot at a known lastUpdateId. Bids/Asks
// are absolute resting levels. Deltas with U <= LastUpdateID are already baked
// into the snapshot and must be dropped during replay (the standard merge rule).
type Snapshot struct {
	Symbol       string
	LastUpdateID int64
	Bids         []Level
	Asks         []Level
}

// Level is one absolute resting price level (exact decimal) in a Snapshot.
type Level struct {
	Price orders.Decimal
	Qty   orders.Decimal
}

// ApplySnapshot DISCARDS any current book contents and rebuilds from snap, setting
// lastU to the snapshot's LastUpdateID. After this the book is ready and a delta is
// contiguous iff delta.PU == lastU. A zero-qty level in a snapshot is ignored (it
// represents nothing resting).
func (b *SequencedBook) ApplySnapshot(snap Snapshot) {
	b.bids = b.bids[:0]
	b.asks = b.asks[:0]
	for _, lv := range snap.Bids {
		if lv.Qty.IsZero() {
			continue
		}
		b.setLevel(Bid, lv.Price, lv.Qty)
	}
	for _, lv := range snap.Asks {
		if lv.Qty.IsZero() {
			continue
		}
		b.setLevel(Ask, lv.Price, lv.Qty)
	}
	b.lastU = snap.LastUpdateID
	b.ready = true
}

// ApplyDelta applies d if it is contiguous with the book's current lastU. It
// returns:
//   - applied == true  when the delta was contiguous and applied (lastU advanced).
//   - applied == false when the delta is STALE (its U <= lastU): already baked in,
//     silently dropped, no error (this is the normal snapshot/delta merge case).
//   - a non-nil *GapError when the delta is non-contiguous in a way that is NOT a
//     stale drop and NOT a snapshot straddle (PU > lastU, U > lastU): a real GAP —
//     at least one update was missed and the book can no longer be trusted.
//
// STRADDLE: the FIRST diff event after a fresh snapshot legitimately does not chain
// exactly (PU == lastU); its batch covers the snapshot boundary (PU <= lastU < U).
// Per Binance's manage-local-order-book procedure this first-post-snapshot event is
// applied, not treated as a gap (see the straddle branch below). After it, strict
// contiguity (PU == lastU) is required again.
//
// The caller MUST treat a GapError as "discard + resnapshot" (GapRecoverer does).
func (b *SequencedBook) ApplyDelta(d Delta) (applied bool, err error) {
	if !b.ready {
		return false, &GapError{Symbol: b.symbol, ExpectedPU: b.lastU, GotPU: d.PU, GotU: d.U, Reason: "book not seeded with a snapshot"}
	}
	// STALE: this delta's whole range is already covered by the snapshot/applied
	// state. Drop it — this is the expected merge behavior, not a gap.
	if d.U <= b.lastU {
		return false, nil
	}
	// CONTIGUOUS: prevUpdateID lines up with our last applied update.
	if d.PU == b.lastU {
		b.setLevel(d.Side, d.Price, d.Qty) // qty 0 removes the level (handled in setLevel)
		b.lastU = d.U
		return true, nil
	}
	// STRADDLE (Binance first-post-snapshot rule): on a real depth feed the FIRST
	// diff event after a REST snapshot does not chain exactly from the snapshot's
	// lastUpdateId — its batch STRADDLES the boundary (PU <= lastU < U). Binance's
	// documented manage-local-order-book procedure is: drop any event whose U is at
	// or below the snapshot lastUpdateId, then apply the first event for which
	// U > lastUpdateId AND PU <= lastUpdateId. That straddling event covers the
	// snapshot boundary, so applying it loses no update — it is NOT a gap. We accept
	// it here (between the strict-contiguous check above and the gap return below),
	// advance lastU to d.U, and from then on a subsequent delta must be STRICTLY
	// contiguous (its PU == lastU) again — handled by the contiguous branch above.
	if d.PU <= b.lastU && d.U > b.lastU {
		b.setLevel(d.Side, d.Price, d.Qty) // qty 0 removes the level (handled in setLevel)
		b.lastU = d.U
		return true, nil
	}
	// Otherwise a real gap: PU does not chain from lastU and U is in the future.
	return false, &GapError{Symbol: b.symbol, ExpectedPU: b.lastU, GotPU: d.PU, GotU: d.U, Reason: "sequence gap"}
}

// GapError reports a non-contiguous delta that cannot be applied: the book has
// missed at least one update and must be discarded and resnapshotted.
type GapError struct {
	Symbol     string
	ExpectedPU int64 // the lastU the next delta's PU had to equal
	GotPU      int64
	GotU       int64
	Reason     string
}

func (e *GapError) Error() string {
	return fmt.Sprintf("data: %s order-book gap (%s): expected pu=%d, got pu=%d u=%d",
		e.Symbol, e.Reason, e.ExpectedPU, e.GotPU, e.GotU)
}

// setLevel sets the absolute resting qty at price on side; qty 0 removes the level.
// Levels are kept sorted by price so iteration / best-of-book is deterministic.
func (b *SequencedBook) setLevel(side Side, price, qty orders.Decimal) {
	book := &b.bids
	if side == Ask {
		book = &b.asks
	}
	levels := *book
	idx := sort.Search(len(levels), func(i int) bool { return levels[i].price.Cmp(price) >= 0 })
	exists := idx < len(levels) && levels[idx].price.Cmp(price) == 0

	if qty.IsZero() {
		if exists { // remove the level
			*book = append(levels[:idx], levels[idx+1:]...)
		}
		return
	}
	if exists { // update in place
		levels[idx].qty = qty
		return
	}
	// insert keeping ascending price order
	levels = append(levels, level{})
	copy(levels[idx+1:], levels[idx:])
	levels[idx] = level{price: price, qty: qty}
	*book = levels
}

// Ready reports whether the book has been seeded with a snapshot.
func (b *SequencedBook) Ready() bool { return b.ready }

// LastUpdateID returns the update ID of the last applied delta (or snapshot).
func (b *SequencedBook) LastUpdateID() int64 { return b.lastU }

// BestBid returns the highest bid price/qty and whether a bid level exists.
func (b *SequencedBook) BestBid() (orders.Decimal, orders.Decimal, bool) {
	if len(b.bids) == 0 {
		return orders.ZeroDecimal, orders.ZeroDecimal, false
	}
	l := b.bids[len(b.bids)-1] // highest price is last (ascending)
	return l.price, l.qty, true
}

// BestAsk returns the lowest ask price/qty and whether an ask level exists.
func (b *SequencedBook) BestAsk() (orders.Decimal, orders.Decimal, bool) {
	if len(b.asks) == 0 {
		return orders.ZeroDecimal, orders.ZeroDecimal, false
	}
	l := b.asks[0] // lowest price is first (ascending)
	return l.price, l.qty, true
}

// QtyAt returns the resting qty at price on side and whether the level exists.
// Used by tests and reconciliation to assert exact book contents after recovery.
func (b *SequencedBook) QtyAt(side Side, price orders.Decimal) (orders.Decimal, bool) {
	levels := b.bids
	if side == Ask {
		levels = b.asks
	}
	idx := sort.Search(len(levels), func(i int) bool { return levels[i].price.Cmp(price) >= 0 })
	if idx < len(levels) && levels[idx].price.Cmp(price) == 0 {
		return levels[idx].qty, true
	}
	return orders.ZeroDecimal, false
}

// ResyncEvent is emitted whenever the order book had to be discarded and rebuilt
// from a fresh snapshot — after a sequence GAP or a reconnect. It signals the rest
// of the system that ORDER/POSITION RECONCILIATION must run: while the book (and
// the feed) was out of sync, fills may have happened that we did not observe, so
// the broker's truth must be re-pulled (ties to slice 3's reconcile-on-boot:
// brokers.ReconcileOnBoot). It implements kernel.Event so it flows through the bus
// like any other event and a reconcile handler can subscribe to it.
type ResyncEvent struct {
	Symbol string
	Reason ResyncReason
	// LastUpdateID is the snapshot update ID the book was rebuilt to.
	LastUpdateID int64
	TS           kernel.UnixNanos
}

// ResyncReason explains why a resync happened.
type ResyncReason int

const (
	// ResyncGap means a sequence gap (missed update) forced the resync.
	ResyncGap ResyncReason = iota
	// ResyncReconnect means a transport reconnect forced the resync.
	ResyncReconnect
)

// String renders the resync reason.
func (r ResyncReason) String() string {
	if r == ResyncReconnect {
		return "reconnect"
	}
	return "gap"
}

// ResyncTopic is the bus topic for resync events, per symbol, e.g. "resync:BTCUSDT".
func ResyncTopic(symbol string) string { return "resync:" + symbol }

// Topic routes resync events per symbol so a reconcile handler can subscribe.
func (e ResyncEvent) Topic() string { return ResyncTopic(e.Symbol) }

// Timestamp is the logical time the resync was performed.
func (e ResyncEvent) Timestamp() kernel.UnixNanos { return e.TS }

// SnapshotFunc fetches a fresh full order-book snapshot for a symbol. It is
// injected into the GapRecoverer so the recovery logic is testable without a live
// REST endpoint (the test supplies a fake; production supplies the venue's REST
// depth-snapshot call).
type SnapshotFunc func(symbol string) (Snapshot, error)

// GapRecoverer drives the standard WebSocket order-book recovery protocol over a
// SequencedBook. Deltas are fed in via Apply; on a GAP or an explicit Reconnect it
// performs the recovery sequence:
//
//	(a) DISCARD the local book,
//	(b) request a fresh SNAPSHOT (via the injected SnapshotFunc),
//	(c) REPLAY buffered deltas after the snapshot — dropping any whose U is already
//	    covered by the snapshot's lastUpdateId, applying the rest contiguously,
//	(d) EMIT a ResyncEvent onto the bus so reconciliation runs.
//
// Deltas that arrive while the book is healthy and contiguous are applied
// directly. A delta that arrives non-contiguously triggers (a)-(d). Because deltas
// can arrive during the snapshot fetch, the recoverer BUFFERS deltas seen since
// the last good state and replays them after the snapshot lands.
//
// Single-owner: a GapRecoverer is driven by one goroutine (the recovery/ingest
// owner). It enqueues ResyncEvents on the bus (Publish), which is the only
// cross-goroutine handoff — dispatch stays single-threaded elsewhere.
type GapRecoverer struct {
	book     *SequencedBook
	snapshot SnapshotFunc
	k        *kernel.Kernel
	symbol   string

	// buffer holds deltas observed since the last applied/known-good update, kept
	// so they can be replayed after a fresh snapshot. It is bounded by the recovery
	// window in practice; we cap it defensively to avoid unbounded growth on a
	// pathological feed.
	buffer    []Delta
	maxBuffer int
}

// NewGapRecoverer builds a recoverer for symbol over book, fetching snapshots via
// snap and emitting ResyncEvents through k (the live entry point — Submit enqueues
// and drains single-threaded). The book starts un-seeded; the first delta (or an
// explicit Reconnect) triggers an initial snapshot+replay.
func NewGapRecoverer(symbol string, book *SequencedBook, snap SnapshotFunc, k *kernel.Kernel) *GapRecoverer {
	return &GapRecoverer{
		book:      book,
		snapshot:  snap,
		k:         k,
		symbol:    symbol,
		maxBuffer: 4096,
	}
}

// Apply feeds one live delta into the recovery state machine. The delta is always
// buffered (so it can be replayed if a resync happens). Then:
//   - if the book is ready and the delta applies contiguously, it is applied and
//     stale buffered deltas are pruned;
//   - if the book is not ready or the delta is non-contiguous (a real gap), a full
//     resync runs (discard → snapshot → replay buffer → ResyncEvent).
//
// at is the logical time to stamp a ResyncEvent with if one is emitted (the venue
// time of this delta). Returns an error only if the snapshot fetch fails during a
// resync (a real, surfaced failure — not a band-aid swallow).
func (g *GapRecoverer) Apply(d Delta, at kernel.UnixNanos) error {
	g.bufferDelta(d)

	if g.book.Ready() {
		applied, err := g.book.ApplyDelta(d)
		if err == nil {
			if applied {
				// Healthy contiguous apply: prune buffered deltas now covered.
				g.pruneBufferThrough(g.book.LastUpdateID())
			}
			// A stale (applied==false, err==nil) delta is simply dropped; the
			// buffer prune on the next contiguous apply will clear it.
			return nil
		}
		// err is a *GapError → fall through to resync.
	}
	return g.resync(ResyncGap, at)
}

// Reconnect is called when the transport reconnected. A reconnect always forces a
// full resync (discard → snapshot → replay buffer → ResyncEvent), because deltas
// were certainly missed while disconnected. It stamps the emitted ResyncEvent with
// `at`.
func (g *GapRecoverer) Reconnect(at kernel.UnixNanos) error {
	return g.resync(ResyncReconnect, at)
}

// resync performs the discard → snapshot → replay → emit sequence and publishes a
// ResyncEvent with the given reason. The snapshot/delta merge drops buffered
// deltas whose U <= snapshot.LastUpdateID (already baked in) and applies the rest
// in order; non-contiguous leftovers after the snapshot are dropped from the
// buffer (they predate the snapshot window) so the book reflects exactly
// snapshot + the deltas that legitimately follow it.
func (g *GapRecoverer) resync(reason ResyncReason, at kernel.UnixNanos) error {
	snap, err := g.snapshot(g.symbol)
	if err != nil {
		return fmt.Errorf("data: %s resync snapshot: %w", g.symbol, err)
	}

	// (a)+(b) discard and rebuild from the fresh snapshot.
	g.book.ApplySnapshot(snap)

	// (c) replay buffered deltas after the snapshot. Sort by U so out-of-arrival
	// buffering still replays in sequence order, then apply each: stale ones (U <=
	// snapshot lastUpdateId) drop, contiguous ones apply, a non-contiguous one ends
	// replay (the remaining buffer predates a usable chain and is discarded — the
	// next live delta will re-trigger recovery if needed).
	sort.SliceStable(g.buffer, func(i, j int) bool { return g.buffer[i].U < g.buffer[j].U })
	replayed := g.buffer
	g.buffer = nil
	for _, d := range replayed {
		applied, derr := g.book.ApplyDelta(d)
		if derr != nil {
			// Non-contiguous against the freshly snapped book: this buffered delta
			// is not part of the post-snapshot chain. Stop replay; later live deltas
			// drive forward (and re-resync if they too gap).
			break
		}
		if applied {
			// keep this delta available for a future resync prune boundary
			g.bufferDelta(d)
		}
	}
	g.pruneBufferThrough(g.book.LastUpdateID())

	// (d) emit the ResyncEvent so reconciliation runs. Submit drives it through the
	// kernel (single-threaded drain) so a subscribed reconcile handler fires.
	g.k.Submit(ResyncEvent{
		Symbol:       g.symbol,
		Reason:       reason,
		LastUpdateID: g.book.LastUpdateID(),
		TS:           at,
	})
	return nil
}

// bufferDelta appends d to the replay buffer, dropping the oldest entry if the cap
// is exceeded (defensive bound; the live recovery window is far smaller).
func (g *GapRecoverer) bufferDelta(d Delta) {
	g.buffer = append(g.buffer, d)
	if len(g.buffer) > g.maxBuffer {
		g.buffer = g.buffer[len(g.buffer)-g.maxBuffer:]
	}
}

// pruneBufferThrough drops buffered deltas whose U <= throughU (already applied),
// keeping the buffer to just the not-yet-superseded tail.
func (g *GapRecoverer) pruneBufferThrough(throughU int64) {
	kept := g.buffer[:0]
	for _, d := range g.buffer {
		if d.U > throughU {
			kept = append(kept, d)
		}
	}
	g.buffer = kept
}
