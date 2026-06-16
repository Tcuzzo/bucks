package risk

import (
	"testing"
	"time"
)

func TestTokenBucket_AllowsBurstThenThrottles(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	// 5 tokens/sec, burst 3, starts full.
	tb := NewTokenBucket(5, 3, clk.now)

	// First 3 immediate requests succeed (the burst).
	for i := 0; i < 3; i++ {
		if !tb.Allow() {
			t.Fatalf("burst request %d should be allowed", i+1)
		}
	}
	// 4th immediate request (no time passed) is throttled.
	if tb.Allow() {
		t.Fatal("4th request with empty bucket and no refill must be throttled")
	}
}

func TestTokenBucket_RefillsAfterInjectedTime(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	// 2 tokens/sec, burst 2.
	tb := NewTokenBucket(2, 2, clk.now)

	// Drain the burst.
	if !tb.Allow() || !tb.Allow() {
		t.Fatal("should allow the 2-token burst")
	}
	if tb.Allow() {
		t.Fatal("bucket should be empty now")
	}

	// Advance 500ms -> 2 tokens/sec * 0.5s = 1 token refilled.
	clk.advance(500 * time.Millisecond)
	if !tb.Allow() {
		t.Fatal("after 500ms a token should have refilled")
	}
	if tb.Allow() {
		t.Fatal("only one token should have refilled in 500ms")
	}

	// Advance 10s -> refills but caps at burst (2), not unbounded.
	clk.advance(10 * time.Second)
	got := tb.Tokens()
	if got > 2.0001 {
		t.Fatalf("refill must cap at burst 2, got %v", got)
	}
	if !tb.Allow() || !tb.Allow() {
		t.Fatal("two tokens should be available after long idle")
	}
	if tb.Allow() {
		t.Fatal("no more than burst tokens after long idle")
	}
}

func TestBackoff_FullJitterWithinBound(t *testing.T) {
	base := 100 * time.Millisecond
	cap := 5 * time.Second
	bo := NewBackoff(base, cap, 42)

	for attempt := 0; attempt < 12; attempt++ {
		// Expected uncapped window = base * 2^attempt, capped at cap.
		window := base
		for i := 0; i < attempt; i++ {
			window *= 2
			if window > cap {
				window = cap
				break
			}
		}
		if window > cap {
			window = cap
		}
		// Sample many times: every sample must be in [0, window].
		for s := 0; s < 200; s++ {
			d := bo.Delay(attempt, 0)
			if d < 0 {
				t.Fatalf("attempt %d: negative delay %v", attempt, d)
			}
			if d > window {
				t.Fatalf("attempt %d: delay %v exceeds full-jitter window %v", attempt, d, window)
			}
			if d > cap {
				t.Fatalf("attempt %d: delay %v exceeds cap %v", attempt, d, cap)
			}
		}
	}
}

func TestBackoff_HonorsRetryAfter(t *testing.T) {
	bo := NewBackoff(100*time.Millisecond, 5*time.Second, 1)

	// A positive Retry-After is returned verbatim (clamped to cap).
	got := bo.Delay(0, 2*time.Second)
	if got != 2*time.Second {
		t.Fatalf("Retry-After must be honored: got %v want 2s", got)
	}

	// Retry-After above cap is clamped to cap.
	got = bo.Delay(3, 30*time.Second)
	if got != 5*time.Second {
		t.Fatalf("Retry-After above cap must clamp to cap: got %v want 5s", got)
	}

	// Zero/negative Retry-After falls through to jitter (must be within window).
	got = bo.Delay(0, 0)
	if got < 0 || got > 100*time.Millisecond {
		t.Fatalf("no Retry-After: attempt 0 delay must be in [0,100ms], got %v", got)
	}
}

func TestBackoff_DeterministicUnderFixedSeed(t *testing.T) {
	// Two backoffs with the SAME seed must produce the SAME delay sequence.
	a := NewBackoff(100*time.Millisecond, 5*time.Second, 12345)
	b := NewBackoff(100*time.Millisecond, 5*time.Second, 12345)

	var seqA, seqB []time.Duration
	for attempt := 0; attempt < 20; attempt++ {
		seqA = append(seqA, a.Delay(attempt, 0))
		seqB = append(seqB, b.Delay(attempt, 0))
	}
	for i := range seqA {
		if seqA[i] != seqB[i] {
			t.Fatalf("same seed must yield same sequence; diverged at %d: %v vs %v",
				i, seqA[i], seqB[i])
		}
	}

	// A DIFFERENT seed should (very likely) produce a different sequence — proves
	// the jitter is actually seed-driven, not constant.
	c := NewBackoff(100*time.Millisecond, 5*time.Second, 99999)
	same := true
	for attempt := 0; attempt < 20; attempt++ {
		if c.Delay(attempt, 0) != seqA[attempt] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different seed produced identical sequence — jitter not seed-driven")
	}
}

func TestBackoff_AttemptZeroBaseWindow(t *testing.T) {
	base := 200 * time.Millisecond
	bo := NewBackoff(base, 10*time.Second, 7)
	// attempt 0 window is exactly base; many samples must stay within [0, base].
	for s := 0; s < 500; s++ {
		d := bo.Delay(0, 0)
		if d < 0 || d > base {
			t.Fatalf("attempt 0 delay %v outside [0, %v]", d, base)
		}
	}
}
