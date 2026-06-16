package kernel

import (
	"errors"
	"testing"
)

// TestNowReflectsLastProcessedEventTime asserts the logical clock equals the
// timestamp of the last processed event, and is the zero value before any event.
func TestNowReflectsLastProcessedEventTime(t *testing.T) {
	k := New()
	if got := k.Now(); got != 0 {
		t.Fatalf("Now() before any event = %s, want 0ns", got)
	}

	// Record Now() observed INSIDE each handler to prove time is advanced before
	// handlers run, and follows event-time.
	var observed []UnixNanos
	k.Bus().Subscribe("signal", func(_ *Bus, _ Event) {
		observed = append(observed, k.Now())
	})

	k.Run([]Event{
		signalEvent{ts: 100, kind: "a"},
		signalEvent{ts: 250, kind: "b"},
		signalEvent{ts: 999, kind: "c"},
	})

	wantObserved := []UnixNanos{100, 250, 999}
	if len(observed) != len(wantObserved) {
		t.Fatalf("observed %d times, want %d", len(observed), len(wantObserved))
	}
	for i := range wantObserved {
		if observed[i] != wantObserved[i] {
			t.Fatalf("observed[%d] = %s, want %s", i, observed[i], wantObserved[i])
		}
	}
	if got := k.Now(); got != 999 {
		t.Fatalf("final Now() = %s, want 999ns", got)
	}
}

// TestNowIsEventTimeNotWallTime proves the kernel uses EVENT time, not wall time:
// we process events whose timestamps are far in the past and far in the future
// (nowhere near real wall-clock nanoseconds) and assert Now() tracks them exactly.
func TestNowIsEventTimeNotWallTime(t *testing.T) {
	k := New()
	k.Bus().Subscribe("signal", func(_ *Bus, _ Event) {})

	// A clearly-not-wall-clock value: year ~1970-adjacent small int, then a large
	// but in-range future value. If the kernel read time.Now() these asserts fail.
	k.Submit(signalEvent{ts: 42, kind: "past"})
	if got := k.Now(); got != 42 {
		t.Fatalf("Now() after past event = %s, want 42ns", got)
	}
	k.Submit(signalEvent{ts: 5_000_000_000_000_000_000, kind: "future"})
	if got := k.Now(); got != 5_000_000_000_000_000_000 {
		t.Fatalf("Now() after future event = %s, want 5e18 ns", got)
	}
}

// TestTimeBackwardPanics asserts an out-of-order (backward) event timestamp is a
// loud failure, not a silent reorder — it panics with ErrTimeWentBackward.
func TestTimeBackwardPanics(t *testing.T) {
	k := New()
	k.Bus().Subscribe("signal", func(_ *Bus, _ Event) {})

	k.Submit(signalEvent{ts: 1000, kind: "first"})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic on backward time, got none")
		}
		var be *ErrTimeWentBackward
		if !errors.As(r.(error), &be) {
			t.Fatalf("panic value = %#v, want *ErrTimeWentBackward", r)
		}
		if be.Last != 1000 || be.Event != 500 {
			t.Fatalf("ErrTimeWentBackward last=%s event=%s, want 1000/500", be.Last, be.Event)
		}
	}()

	k.Submit(signalEvent{ts: 500, kind: "backward"}) // earlier than 1000 -> panic
}

// TestUnixNanosOverflowPanics asserts UnixNanos.Add panics on signed-64-bit
// overflow rather than silently wrapping, per the spec.
func TestUnixNanosOverflowPanics(t *testing.T) {
	cases := []struct {
		name string
		base UnixNanos
		add  int64
	}{
		{"max plus one", MaxUnixNanos, 1},
		{"near-max plus large", MaxUnixNanos - 10, 100},
		{"min minus one", MinUnixNanos, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("expected overflow panic for %s + %d", tc.base, tc.add)
				}
				var oe *ErrUnixNanosOverflow
				if !errors.As(r.(error), &oe) {
					t.Fatalf("panic value = %#v, want *ErrUnixNanosOverflow", r)
				}
			}()
			_ = tc.base.Add(tc.add)
		})
	}
}

// TestUnixNanosAddNoOverflow asserts Add is correct (and does NOT panic) for
// in-range arithmetic, including crossing zero.
func TestUnixNanosAddNoOverflow(t *testing.T) {
	cases := []struct {
		base UnixNanos
		add  int64
		want UnixNanos
	}{
		{0, 0, 0},
		{100, 50, 150},
		{100, -150, -50},
		{MaxUnixNanos - 5, 5, MaxUnixNanos},
		{MinUnixNanos + 5, -5, MinUnixNanos},
	}
	for _, tc := range cases {
		if got := tc.base.Add(tc.add); got != tc.want {
			t.Fatalf("%s.Add(%d) = %s, want %s", tc.base, tc.add, got, tc.want)
		}
	}
}

// TestSameTimestampIsAllowed asserts two events at the SAME logical time are fine
// (non-decreasing, not strictly increasing): no panic, Now stays put.
func TestSameTimestampIsAllowed(t *testing.T) {
	k := New()
	var hits int
	k.Bus().Subscribe("signal", func(_ *Bus, _ Event) { hits++ })

	k.Run([]Event{
		signalEvent{ts: 7, kind: "a"},
		signalEvent{ts: 7, kind: "b"},
		signalEvent{ts: 7, kind: "c"},
	})
	if hits != 3 {
		t.Fatalf("hits = %d, want 3", hits)
	}
	if k.Now() != 7 {
		t.Fatalf("Now() = %s, want 7ns", k.Now())
	}
}

// TestBacktestAndLiveClockProduceSameTime asserts the SAME engine driven by the
// BacktestClock and by the LiveClock over identical events reaches identical
// logical time — one engine, two clocks.
func TestBacktestAndLiveClockProduceSameTime(t *testing.T) {
	events := []Event{
		signalEvent{ts: 10, kind: "a"},
		signalEvent{ts: 20, kind: "b"},
		signalEvent{ts: 30, kind: "c"},
	}

	kb := New()
	kb.Bus().Subscribe("signal", func(_ *Bus, _ Event) {})
	bt := NewBacktestClock(events)
	gotBT := bt.Drive(kb)

	kl := New()
	kl.Bus().Subscribe("signal", func(_ *Bus, _ Event) {})
	live := NewLiveClock()
	for _, e := range events {
		live.Feed(e)
	}
	gotLive := live.Drive(kl)

	if gotBT != gotLive {
		t.Fatalf("backtest final time %s != live final time %s", gotBT, gotLive)
	}
	if gotBT != 30 {
		t.Fatalf("final time = %s, want 30ns", gotBT)
	}
}
