package kernel

import (
	"reflect"
	"testing"
)

// TestBusDeliversToRightHandlersByTopic asserts an event is delivered only to the
// handlers subscribed to its topic, and not to other topics' handlers.
func TestBusDeliversToRightHandlersByTopic(t *testing.T) {
	b := NewBus()
	var quoteHits, signalHits int
	b.Subscribe("quote", func(_ *Bus, _ Event) { quoteHits++ })
	b.Subscribe("signal", func(_ *Bus, _ Event) { signalHits++ })

	b.Publish(quoteEvent{ts: 1, symbol: "BTC", px: mustDec("100")})
	b.Publish(signalEvent{ts: 2, kind: "go"})
	b.Publish(quoteEvent{ts: 3, symbol: "BTC", px: mustDec("101")})
	b.drain()

	if quoteHits != 2 {
		t.Fatalf("quote handler hits = %d, want 2", quoteHits)
	}
	if signalHits != 1 {
		t.Fatalf("signal handler hits = %d, want 1", signalHits)
	}
}

// TestBusFIFOOrderPreserved asserts events are dispatched strictly in the order
// they were published (FIFO), single-threaded.
func TestBusFIFOOrderPreserved(t *testing.T) {
	b := NewBus()
	var got []string
	b.Subscribe("signal", func(_ *Bus, e Event) {
		got = append(got, e.(signalEvent).kind)
	})

	want := []string{"a", "b", "c", "d", "e"}
	for i, k := range want {
		b.Publish(signalEvent{ts: UnixNanos(i + 1), kind: k})
	}
	b.drain()

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FIFO order = %v, want %v", got, want)
	}
}

// TestBusBatchDrainCascadeDeferred documents the LOW-LEVEL bare-Bus batch-publish
// semantics ONLY — it is NOT the Kernel's production contract. When two source
// events are published into the bus together (a batch) and THEN drained once, a
// cascade published by a handler enqueues at the TAIL of the shared FIFO queue.
// So both batched source events fully process (enter+exit) before EITHER cascade
// runs, simply because both source events were already ahead of the cascades in
// the one queue.
//
// This is a property of calling drain() on a pre-filled batch, not of how the
// Kernel runs. The Kernel drains per source event (see
// TestKernelCascadeOrderProductionPath), so in production each source event's
// cascade resolves before the NEXT source event — a different, monotonic order.
func TestBusBatchDrainCascadeDeferred(t *testing.T) {
	b := NewBus()
	var seq []string

	b.Subscribe("signal", func(bus *Bus, e Event) {
		seq = append(seq, "signal:enter:"+e.(signalEvent).kind)
		// Publish a cascade from inside the handler. It must NOT run now.
		bus.Publish(cascadeEvent{ts: e.Timestamp(), tag: e.(signalEvent).kind})
		seq = append(seq, "signal:exit:"+e.(signalEvent).kind)
	})
	b.Subscribe("cascade", func(_ *Bus, e Event) {
		seq = append(seq, "cascade:"+e.(cascadeEvent).tag)
	})

	// Both source events are batched into the queue, THEN drained once.
	b.Publish(signalEvent{ts: 1, kind: "x"})
	b.Publish(signalEvent{ts: 2, kind: "y"})
	b.drain()

	// Because both source events were enqueued ahead of any cascade, both fully
	// process (enter+exit) before EITHER cascade runs. This is bare-Bus
	// batch-drain behavior — NOT the per-source-event drain the Kernel uses.
	want := []string{
		"signal:enter:x", "signal:exit:x",
		"signal:enter:y", "signal:exit:y",
		"cascade:x",
		"cascade:y",
	}
	if !reflect.DeepEqual(seq, want) {
		t.Fatalf("bare-Bus batch-drain ordering =\n  %v\nwant\n  %v", seq, want)
	}
}

// TestKernelCascadeOrderProductionPath asserts the REAL production ordering. A
// Kernel drains the bus per source event (Run/Submit publish ONE source event then
// drain to completion before the next), so each source event's cascade resolves
// BEFORE the next source event is processed. For two source events x then y, each
// spawning a cascade, the handler order is:
//
//	[enter:x, exit:x, cascade:x, enter:y, exit:y, cascade:y]
//
// This is the contract callers rely on — cascades stay at their source event's
// logical time and never interleave across the next tick. It differs from the
// bare-Bus batch-drain shape in TestBusBatchDrainCascadeDeferred.
func TestKernelCascadeOrderProductionPath(t *testing.T) {
	k := New()
	var seq []string

	k.Bus().Subscribe("signal", func(bus *Bus, e Event) {
		seq = append(seq, "signal:enter:"+e.(signalEvent).kind)
		// Publish a cascade from inside the handler.
		bus.Publish(cascadeEvent{ts: e.Timestamp(), tag: e.(signalEvent).kind})
		seq = append(seq, "signal:exit:"+e.(signalEvent).kind)
	})
	k.Bus().Subscribe("cascade", func(_ *Bus, e Event) {
		seq = append(seq, "cascade:"+e.(cascadeEvent).tag)
	})

	// Drive through the Kernel exactly as production does: an ordered batch fed to
	// Run, which drains per source event.
	k.Run([]Event{
		signalEvent{ts: 1, kind: "x"},
		signalEvent{ts: 2, kind: "y"},
	})

	want := []string{
		"signal:enter:x", "signal:exit:x",
		"cascade:x",
		"signal:enter:y", "signal:exit:y",
		"cascade:y",
	}
	if !reflect.DeepEqual(seq, want) {
		t.Fatalf("kernel cascade ordering =\n  %v\nwant\n  %v", seq, want)
	}
}

// TestBusOrderStableRegardlessOfSubscriptionOrder asserts handlers for a topic
// fire in REGISTRATION order (not Go map order) and that the dispatch is stable.
// We register three handlers and assert they always run a, b, c in that order
// across repeated drains.
func TestBusHandlersFireInRegistrationOrder(t *testing.T) {
	b := NewBus()
	var order []string
	b.Subscribe("signal", func(_ *Bus, _ Event) { order = append(order, "a") })
	b.Subscribe("signal", func(_ *Bus, _ Event) { order = append(order, "b") })
	b.Subscribe("signal", func(_ *Bus, _ Event) { order = append(order, "c") })

	b.Publish(signalEvent{ts: 1, kind: "go"})
	b.drain()

	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("handler order = %v, want %v", order, want)
	}
}

// TestBusDeepCascadeNoStackBlowup asserts the iterative drain handles a long
// cascade chain (each event spawns the next) without recursion / stack growth.
func TestBusDeepCascadeNoStackBlowup(t *testing.T) {
	b := NewBus()
	const depth = 100000
	var count int
	b.Subscribe("cascade", func(bus *Bus, e Event) {
		count++
		c := e.(cascadeEvent)
		if count < depth {
			bus.Publish(cascadeEvent{ts: c.ts, tag: c.tag})
		}
	})

	b.Publish(cascadeEvent{ts: 1, tag: "chain"})
	b.drain()

	if count != depth {
		t.Fatalf("processed %d cascade events, want %d", count, depth)
	}
}

// TestBusReentrantDrainIsNoop asserts a handler that (incorrectly) calls drain
// does not reorder events: the inner drain is a guarded no-op and the outer drain
// still processes FIFO.
func TestBusReentrantDrainIsNoop(t *testing.T) {
	b := NewBus()
	var got []string
	b.Subscribe("signal", func(bus *Bus, e Event) {
		got = append(got, e.(signalEvent).kind)
		bus.drain() // re-entrant: must be a no-op
	})

	b.Publish(signalEvent{ts: 1, kind: "first"})
	b.Publish(signalEvent{ts: 2, kind: "second"})
	b.drain()

	want := []string{"first", "second"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("re-entrant drain reordered events: got %v, want %v", got, want)
	}
}
