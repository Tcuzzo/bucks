package kernel

import "sort"

// Event is anything that flows through the bus. Every event is timestamped in
// LOGICAL time (event-time, NOT wall-time) and names its topic. The kernel uses
// the timestamp to advance its logical clock and the topic to route the event to
// the handlers subscribed to that topic.
//
// Implementations are expected to be immutable value-like payloads: once
// published, an event is not mutated by handlers. (Handlers mutate the cache and
// publish NEW events; they do not edit events in flight.) This keeps replay
// deterministic.
type Event interface {
	// Topic is the routing key. Handlers subscribe by topic.
	Topic() string
	// Timestamp is the event-time of this event in logical nanoseconds. The
	// kernel advances Now() to this value when the event is processed.
	Timestamp() UnixNanos
}

// Handler reacts to an event. A handler runs synchronously on the single dispatch
// goroutine and may publish further events via the supplied *Bus; those cascade
// events enqueue and are processed in deterministic FIFO order AFTER the current
// event (and after any earlier-published cascades), never re-entrantly.
//
// A handler MUST NOT start goroutines that touch the bus, cache, or clock, and
// MUST NOT read the wall clock — doing so would break replay determinism.
type Handler func(b *Bus, e Event)

// subscription pairs a handler with a stable registration sequence number. We
// dispatch in ascending seq order (registration order), NEVER Go map-iteration
// order, so the handler firing sequence is identical on every run.
type subscription struct {
	seq     uint64
	handler Handler
}

// Bus is the in-memory typed pub/sub + command bus with SINGLE-THREADED FIFO
// dispatch. Its invariants (the source of replay determinism):
//
//  1. Exactly one event is dispatched at a time, drained from an explicit FIFO
//     queue. There is no concurrent handler execution and no recursion: a handler
//     that publishes enqueues; the queue is drained iteratively.
//  2. Handlers for a topic fire in registration order (by seq), not map order.
//  3. The bus reads no wall clock and contains no randomness.
//
// The Bus is normally driven by the Kernel (which owns time). It is exported so
// handlers can Publish cascade events; the Kernel calls drain.
//
// Concurrency note: Publish may be called from a producer goroutine (e.g. a live
// market-data feed) to ENQUEUE an event; that enqueue is guarded so the queue is
// never corrupted. DISPATCH itself is still strictly single-threaded — only the
// kernel's run loop drains and invokes handlers, one event at a time. Handlers
// therefore never execute concurrently regardless of how many producers enqueue.
type Bus struct {
	// subscribers maps topic -> ordered subscriptions. Map iteration order is
	// never relied upon: we always dispatch a single topic's slice in seq order.
	subscribers map[string][]subscription
	nextSeq     uint64

	// queue is the explicit FIFO event queue. Draining is iterative (no
	// recursion) so deep cascades cannot blow the stack.
	queue []Event

	// dispatching guards against re-entrant drain: a handler that (mis)calls
	// drain would otherwise reorder events. Only the outermost drain runs.
	dispatching bool

	// onDispatch, if set, is invoked by the kernel for every event right before
	// its handlers run, so the kernel can advance its logical clock to the
	// event's timestamp. Kept on the bus so Publish/drain stay one mechanism.
	onDispatch func(e Event)
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{
		subscribers: make(map[string][]subscription),
	}
}

// Subscribe registers handler for the given topic. Handlers are dispatched in the
// order they were subscribed (stable registration order). Returns nothing — the
// kernel model has no dynamic unsubscribe in the hot path (deterministic wiring
// is set up once before replay).
func (b *Bus) Subscribe(topic string, handler Handler) {
	b.subscribers[topic] = append(b.subscribers[topic], subscription{
		seq:     b.nextSeq,
		handler: handler,
	})
	b.nextSeq++
}

// Publish enqueues e for dispatch. It does NOT run handlers synchronously — the
// event is appended to the FIFO queue and processed when the queue is drained.
// This is what gives cascades deterministic ordering: a handler that publishes
// during dispatch simply appends, and the appended event runs after the current
// one in the same drain.
func (b *Bus) Publish(e Event) {
	b.queue = append(b.queue, e)
}

// drain processes the queue to completion in FIFO order, single-threaded and
// iteratively (no recursion). For each event it (a) notifies onDispatch so the
// kernel can advance logical time, then (b) invokes that topic's handlers in
// stable registration order. Handlers may Publish; those events are appended and
// drained in turn. Re-entrant drain is a no-op guarded by `dispatching`.
func (b *Bus) drain() {
	if b.dispatching {
		return
	}
	b.dispatching = true
	defer func() { b.dispatching = false }()

	for len(b.queue) > 0 {
		// Pop the head (FIFO). Slice the head off; the underlying array reuse is
		// fine because we only ever read forward.
		e := b.queue[0]
		b.queue = b.queue[1:]

		if b.onDispatch != nil {
			b.onDispatch(e)
		}

		// Dispatch to handlers in stable registration order. We copy the slice
		// header (not the elements) so a handler that subscribes mid-dispatch
		// does not retroactively receive the in-flight event.
		subs := b.subscribers[e.Topic()]
		for _, s := range subs {
			s.handler(b, e)
		}
	}
	// Once fully drained, compact the (now-empty) queue back to a fresh slice so
	// the popped-off backing array can be GC'd between drains.
	b.queue = nil
}

// topics returns the registered topics in sorted order. Used only by diagnostics
// and tests — never by dispatch — but it sorts so callers that DO iterate topics
// get a deterministic order rather than Go's randomized map iteration.
func (b *Bus) topics() []string {
	out := make([]string, 0, len(b.subscribers))
	for t := range b.subscribers {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
