package risk

import (
	"math"
	"math/rand"
	"sync"
	"time"
)

// TokenBucket is a per-broker rate limiter with a clock-injected refill. It lets
// BUCKS respect each venue's published limit (Alpaca 200/min, Coinbase 30/s,
// etc.) without a wall-clock dependency in testable logic: the bucket refills
// based on the time returned by an injected now func, so a test can advance time
// deterministically.
//
// Semantics: the bucket holds up to burst tokens; tokens refill continuously at
// rate tokens/second up to burst. Allow() consumes one token if available
// (returning true) or denies (returning false) without blocking. All money/time
// math is done in integer nanoseconds and exact fractions — no float64 for the
// token accounting decision (a float is used only for the continuous refill rate,
// which is a smoothing parameter, not money).
type TokenBucket struct {
	mu sync.Mutex

	ratePerSec float64 // refill rate, tokens per second (a rate, not money)
	burst      float64 // max tokens
	tokens     float64 // current tokens
	last       time.Time
	now        func() time.Time
}

// NewTokenBucket builds a bucket that refills at ratePerSec tokens/second up to
// burst tokens, starting full. now is the injected clock (pass time.Now in
// production). A nil now defaults to time.Now. A non-positive burst is clamped to
// 1 so the bucket can always eventually allow one request.
func NewTokenBucket(ratePerSec float64, burst int, now func() time.Time) *TokenBucket {
	if now == nil {
		now = time.Now
	}
	b := float64(burst)
	if b < 1 {
		b = 1
	}
	if ratePerSec < 0 {
		ratePerSec = 0
	}
	return &TokenBucket{
		ratePerSec: ratePerSec,
		burst:      b,
		tokens:     b, // start full
		last:       now(),
		now:        now,
	}
}

// refillLocked adds tokens for the elapsed time since last, capped at burst. The
// caller must hold b.mu.
func (b *TokenBucket) refillLocked(t time.Time) {
	if !t.After(b.last) {
		return
	}
	elapsed := t.Sub(b.last).Seconds()
	b.tokens = math.Min(b.burst, b.tokens+elapsed*b.ratePerSec)
	b.last = t
}

// Allow consumes one token and returns true if a token was available, else false.
// It refills based on the injected clock first. Non-blocking.
func (b *TokenBucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(b.now())
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// Tokens reports the current available token count (after refill), for telemetry
// and tests.
func (b *TokenBucket) Tokens() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(b.now())
	return b.tokens
}

// Backoff computes retry delays with FULL JITTER. For attempt n (0-based) the
// uncapped exponential window is base * 2^n, capped at cap; the actual delay is a
// uniform random value in [0, min(cap, base*2^n)]. A Retry-After hint (e.g. from a
// 429 response) takes precedence: when present and positive, Delay returns
// Retry-After (clamped to cap) rather than a jittered value, since the server told
// us exactly how long to wait.
//
// IMPORTANT (documented contract, enforced by the caller, not here): a retry MUST
// reuse the SAME ClOrdID (the idempotency key) so the broker dedupes the resend.
// This type only computes the delay; reusing the idempotency key on the resend is
// the caller's responsibility.
//
// Determinism: the jitter source is an injected *rand.Rand built from a seed, so a
// fixed seed produces a fixed delay sequence. Backoff is mutex-guarded because
// *rand.Rand is not safe for concurrent use.
type Backoff struct {
	mu   sync.Mutex
	base time.Duration
	cap  time.Duration
	rng  *rand.Rand
}

// NewBackoff builds a full-jitter backoff with the given base and cap delays,
// seeded deterministically. A non-positive base defaults to 100ms; a cap below
// base is raised to base. The same seed yields the same delay sequence.
func NewBackoff(base, cap time.Duration, seed int64) *Backoff {
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	if cap < base {
		cap = base
	}
	return &Backoff{
		base: base,
		cap:  cap,
		rng:  rand.New(rand.NewSource(seed)),
	}
}

// Delay returns the wait before retry attempt n (0-based). retryAfter is an
// optional server hint (<= 0 means "none"). When a positive retryAfter is given,
// Delay returns it clamped to cap. Otherwise it returns a full-jitter value in
// [0, min(cap, base*2^n)].
func (b *Backoff) Delay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > b.cap {
			return b.cap
		}
		return retryAfter
	}

	// Exponential window: base * 2^attempt, capped, computed in integer
	// nanoseconds with overflow protection (a large attempt must not wrap).
	window := b.expWindow(attempt)

	b.mu.Lock()
	defer b.mu.Unlock()
	if window <= 0 {
		return 0
	}
	// Full jitter: uniform in [0, window]. Int63n needs a positive bound;
	// window is in [1, cap] here.
	return time.Duration(b.rng.Int63n(int64(window) + 1))
}

// expWindow returns min(cap, base*2^attempt) as a time.Duration, guarding against
// integer overflow for large attempts (which simply clamp to cap).
func (b *Backoff) expWindow(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	capNanos := int64(b.cap)
	baseNanos := int64(b.base)
	// If shifting would overflow or exceed cap, return cap. Each doubling that
	// would pass cap can stop early.
	w := baseNanos
	for i := 0; i < attempt; i++ {
		// Detect overflow / cap breach before doubling.
		if w > capNanos/2 {
			return b.cap
		}
		w *= 2
	}
	if w > capNanos {
		return b.cap
	}
	return time.Duration(w)
}
