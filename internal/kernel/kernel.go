package kernel

import "fmt"

// Kernel is the deterministic engine: it owns the bus, the (implicit) event
// queue inside the bus, the cache, and a LOGICAL clock. The same Kernel code runs
// for backtest and for live — only the SOURCE of events differs (a BacktestClock
// replays historical event timestamps; a LiveClock carries timestamps off real
// incoming events). Because the engine is identical, paper and live can never
// diverge in how they process a given ordered sequence of events.
//
// Time discipline (enforced, not just documented):
//   - The kernel NEVER reads the wall clock. There is no time.Now() anywhere on
//     the dispatch path.
//   - Logical time advances ONLY when an event is processed, to that event's
//     Timestamp(). Now() returns the last processed event time.
//   - Now() is monotonic non-decreasing: an event whose timestamp is older than
//     the last processed time is rejected (out-of-order events must be sequenced
//     by the event source, not silently reordered here).
type Kernel struct {
	bus   *Bus
	cache *Cache

	// now is the logical clock: the timestamp of the last processed event. It
	// starts unset; the first processed event establishes it.
	now    UnixNanos
	nowSet bool
}

// ErrTimeWentBackward is returned (as a panic value via dispatch wiring) when an
// event arrives with a timestamp earlier than the last processed event time. We
// surface it loudly because silently accepting it would corrupt event-time
// ordering and replay determinism. Event sources must deliver in non-decreasing
// timestamp order.
type ErrTimeWentBackward struct {
	Last  UnixNanos
	Event UnixNanos
	Topic string
}

func (e *ErrTimeWentBackward) Error() string {
	return fmt.Sprintf("kernel: event time went backward on topic %q: last=%s event=%s",
		e.Topic, e.Last, e.Event)
}

// New constructs a kernel with a fresh bus and cache and wires the bus's
// per-event hook to advance the logical clock. The wiring is the single seam
// where event-time becomes the clock.
func New() *Kernel {
	k := &Kernel{
		bus:   NewBus(),
		cache: NewCache(),
	}
	k.bus.onDispatch = k.advanceTo
	return k
}

// Bus returns the kernel's bus so components can Subscribe before the run and so
// handlers (which already receive *Bus) line up with the same instance.
func (k *Kernel) Bus() *Bus { return k.bus }

// Cache returns the kernel's cache.
func (k *Kernel) Cache() *Cache { return k.cache }

// Now returns the logical time of the last processed event. Before any event has
// been processed it returns the zero UnixNanos. It NEVER reads the wall clock.
func (k *Kernel) Now() UnixNanos { return k.now }

// advanceTo moves the logical clock to the event's timestamp. It is invoked by
// the bus immediately before an event's handlers run, so handlers observe Now()
// already at the current event's time. Time is monotonic: a backward jump is a
// programming/feed error and panics rather than corrupting ordering.
func (k *Kernel) advanceTo(e Event) {
	ts := e.Timestamp()
	if k.nowSet && ts < k.now {
		panic(&ErrTimeWentBackward{Last: k.now, Event: ts, Topic: e.Topic()})
	}
	k.now = ts
	k.nowSet = true
}

// Submit enqueues a single event and drains the bus to completion (processing the
// event and any cascade events its handlers publish), single-threaded and in FIFO
// order. It is the live-path entry point: a LiveClock source calls Submit for each
// incoming real event. Run is exactly Submit applied to an ordered batch, so the
// backtest and live paths execute the identical per-event drain logic.
func (k *Kernel) Submit(e Event) {
	k.bus.Publish(e)
	k.bus.drain()
}

// Run feeds an ordered slice of events through the kernel ONE AT A TIME, draining
// the bus (the source event plus every cascade it spawns) before the next source
// event is processed. This is the backtest-path entry point: a BacktestClock hands
// the kernel the historical events in order.
//
// Why one-at-a-time, not batch-enqueue-then-drain: a cascade event carries the
// LOGICAL time of the source event that spawned it (event-time). If the whole
// batch were enqueued first, a later source event would advance the clock past a
// cascade still waiting in the queue, making the cascade appear to travel back in
// time. Draining per source event keeps logical time monotonic and matches how a
// real event source (and the live path, Submit) delivers events: each event fully
// resolves — including its cascade — at its own time before the next tick.
//
// Determinism: events are processed in the given order and dispatched FIFO; the
// resulting state depends ONLY on the event sequence and the (registration-order)
// handler wiring — not on wall time, not on map iteration order. This is exactly
// what Submit does for a single event; Run is Submit applied to an ordered batch.
func (k *Kernel) Run(events []Event) {
	for _, e := range events {
		k.bus.Publish(e)
		k.bus.drain()
	}
}

// Clock is the seam that distinguishes backtest from live WITHOUT changing the
// engine. A Clock produces the next event(s) for the kernel to process; the
// kernel's processing of those events is identical regardless of which Clock fed
// them. Drive() pushes this clock's events through the supplied kernel.
type Clock interface {
	// Drive feeds this clock's events through k. For a backtest this replays the
	// whole prepared sequence; for live it would block on a feed (modeled here by
	// draining whatever events have been queued). It returns the kernel's logical
	// time after processing.
	Drive(k *Kernel) UnixNanos
}

// BacktestClock replays a fixed, pre-recorded sequence of historical events. Time
// "advances" purely by the timestamps carried on those events — there is no
// sleeping and no wall clock. The exact same kernel processes them as in live.
type BacktestClock struct {
	events []Event
}

// NewBacktestClock builds a backtest clock over the given ordered events. The
// caller is responsible for supplying them in non-decreasing timestamp order
// (the kernel will panic on a backward jump, which is the intended loud failure).
func NewBacktestClock(events []Event) *BacktestClock {
	return &BacktestClock{events: events}
}

// Drive replays all events through k in order and returns the final logical time.
func (c *BacktestClock) Drive(k *Kernel) UnixNanos {
	k.Run(c.events)
	return k.Now()
}

// LiveClock carries timestamps straight off real incoming events: there is no
// historical replay, the timestamp on each event IS the clock. In production a
// feed goroutine would call Feed for each arriving event; here Feed enqueues and
// Drive drains, so the SAME kernel logic that ran the backtest runs live. The
// only difference from BacktestClock is where the events come from.
type LiveClock struct {
	pending []Event
}

// NewLiveClock returns an empty live clock.
func NewLiveClock() *LiveClock {
	return &LiveClock{}
}

// Feed appends a real incoming event to be processed on the next Drive. (In a
// real deployment the feed thread enqueues onto the bus directly; this models the
// arrival path so backtest and live share one code shape.)
func (c *LiveClock) Feed(e Event) {
	c.pending = append(c.pending, e)
}

// Drive processes all events fed so far through k, in arrival order, and returns
// the final logical time. The kernel code path is identical to the backtest one.
func (c *LiveClock) Drive(k *Kernel) UnixNanos {
	k.Run(c.pending)
	c.pending = nil
	return k.Now()
}
