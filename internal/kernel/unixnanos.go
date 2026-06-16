// Package kernel is BUCKS's deterministic event kernel — the single-threaded
// heart that runs the SAME way in backtest and live so paper and live behavior
// can never diverge. It holds three things: an in-memory message bus (typed
// pub/sub with single-threaded FIFO dispatch), a logical event-time clock, and a
// write-then-published cache.
//
// This is BUCKS's own redesign of the event-driven-engine pattern (studied, not
// vendored — see the build spec §4.1 / §11 Attribution). The design rules that
// make replay bit-identical are documented at each seam and enforced in code:
//
//   - Single-threaded dispatch. Events are processed one at a time, in FIFO order,
//     from an explicit queue. No goroutines in the hot dispatch path; no concurrent
//     handler execution. A handler may publish further events (a cascade) — those
//     enqueue and process deterministically after the current one.
//   - Event-time, not wall-time. The kernel NEVER reads the wall clock. Time
//     advances only when an event carrying a timestamp is processed. Now() returns
//     the last processed event time.
//   - No map-iteration-order dependence in dispatch. Handlers fire in their stable
//     registration order, not Go map order, so the same events produce the same
//     handler sequence on every run.
//   - No randomness in the kernel itself. If a component needs randomness it must
//     take an explicit seed; the kernel core needs none.
package kernel

import (
	"fmt"
	"math"
)

// UnixNanos is a logical timestamp: nanoseconds since the Unix epoch, as a signed
// 64-bit integer. It is the ONLY notion of time inside the kernel.
//
// We model time as a typed int64 that PANICS on overflow rather than silently
// wrapping (per the build spec — "time as typed UnixNanos that panics on
// overflow"). A silent wrap would move time backwards and destroy determinism /
// ordering guarantees, so it is a programming error we surface loudly, not hide.
type UnixNanos int64

// MaxUnixNanos is the largest representable logical timestamp. Year ~2262.
const MaxUnixNanos UnixNanos = math.MaxInt64

// MinUnixNanos is the smallest representable logical timestamp.
const MinUnixNanos UnixNanos = math.MinInt64

// String renders the raw nanosecond count. Deliberately not a wall-clock
// rendering: the kernel deals in logical time only.
func (t UnixNanos) String() string {
	return fmt.Sprintf("%dns", int64(t))
}

// ErrUnixNanosOverflow is the typed value panicked when UnixNanos.Add would leave
// the representable signed-64-bit range (positive or negative). It mirrors
// ErrTimeWentBackward: a typed, exported error surfaced loudly via panic so a
// recover() can identify the failure precisely rather than matching a string. A
// silent wrap would move logical time and destroy ordering/determinism, so it is a
// programming error we never hide.
type ErrUnixNanosOverflow struct {
	Base   int64
	Add    int64
	Signed int // +1 for positive overflow, -1 for negative overflow.
}

func (e *ErrUnixNanosOverflow) Error() string {
	dir := "positive"
	if e.Signed < 0 {
		dir = "negative"
	}
	return fmt.Sprintf("kernel: UnixNanos %s overflow: %d + %d", dir, e.Base, e.Add)
}

// Add returns t + d (d in nanoseconds), panicking on signed-64-bit overflow
// instead of wrapping. Overflow here means the logical clock would jump past the
// representable range — a bug, never a normal condition — so we fail loud with a
// typed *ErrUnixNanosOverflow (mirroring *ErrTimeWentBackward).
func (t UnixNanos) Add(d int64) UnixNanos {
	a := int64(t)
	sum := a + d
	// Signed overflow detection: the sign flipped in a way addition cannot
	// produce unless the true result left the int64 range.
	if d > 0 && sum < a {
		panic(&ErrUnixNanosOverflow{Base: a, Add: d, Signed: +1})
	}
	if d < 0 && sum > a {
		panic(&ErrUnixNanosOverflow{Base: a, Add: d, Signed: -1})
	}
	return UnixNanos(sum)
}
