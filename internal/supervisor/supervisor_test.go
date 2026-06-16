package supervisor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock is a deterministic Clock: After(d) records d and returns a channel that
// the test fires manually via release(), so no real time passes in the suite.
type fakeClock struct {
	mu       sync.Mutex
	requests []time.Duration  // every duration After was asked for, in order
	pending  []chan time.Time // channels awaiting release
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	c.requests = append(c.requests, d)
	c.pending = append(c.pending, ch)
	c.mu.Unlock()
	return ch
}

// release fires the next pending timer (FIFO), unblocking one backoff wait.
func (c *fakeClock) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return
	}
	ch := c.pending[0]
	c.pending = c.pending[1:]
	ch <- time.Now()
}

func (c *fakeClock) durations() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, len(c.requests))
	copy(out, c.requests)
	return out
}

func (c *fakeClock) pendingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

// flakyComponent fails the first failTimes Run calls, then succeeds (returns nil).
type flakyComponent struct {
	name      string
	failTimes int
	mu        sync.Mutex
	calls     int
}

func (f *flakyComponent) Name() string { return f.name }

func (f *flakyComponent) Run(ctx context.Context) error {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if n <= f.failTimes {
		return errors.New("flaky boom")
	}
	return nil
}

func (f *flakyComponent) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// permaComponent always fails.
type permaComponent struct {
	name  string
	mu    sync.Mutex
	calls int
}

func (p *permaComponent) Name() string { return p.name }

func (p *permaComponent) Run(ctx context.Context) error {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return errors.New("permanently broken")
}

func (p *permaComponent) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// superviseAsync runs Supervise in a goroutine and returns a func that blocks for
// the result. The test drives the fake clock while it runs.
func superviseAsync(s *Supervisor, ctx context.Context, c Component) func() (Outcome, error) {
	type res struct {
		out Outcome
		err error
	}
	done := make(chan res, 1)
	go func() {
		out, err := s.Supervise(ctx, c)
		done <- res{out, err}
	}()
	return func() (Outcome, error) {
		r := <-done
		return r.out, r.err
	}
}

// waitForPending spins (briefly, with real-time yield) until the fake clock has at
// least n pending timers — i.e. the supervisor has reached its next backoff wait.
func waitForPending(t *testing.T, c *fakeClock, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.pendingCount() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d pending timers (have %d)", n, c.pendingCount())
}

// TestFlakyComponentRecovers: a component that fails twice then succeeds is
// restarted and recovers, with exactly 2 restarts.
func TestFlakyComponentRecovers(t *testing.T) {
	clk := &fakeClock{}
	s := New(clk, Policy{MaxRestarts: 5, BaseBackoff: 10 * time.Millisecond, Factor: 2})
	c := &flakyComponent{name: "feed", failTimes: 2}

	wait := superviseAsync(s, context.Background(), c)

	// Two failures -> two backoff waits; release each so the supervisor restarts.
	waitForPending(t, clk, 1)
	clk.release()
	waitForPending(t, clk, 1) // second backoff is requested after the second failure
	clk.release()

	out, err := wait()
	if err != nil {
		t.Fatalf("expected recovery, got err: %v", err)
	}
	if !out.Recovered {
		t.Fatalf("expected Recovered=true, got %+v", out)
	}
	if out.Restarts != 2 {
		t.Fatalf("expected 2 restarts, got %d", out.Restarts)
	}
	if c.callCount() != 3 {
		t.Fatalf("expected 3 Run calls (2 fail + 1 ok), got %d", c.callCount())
	}
}

// TestPermanentFailureHitsCap: an always-failing component stops after MaxRestarts
// — no infinite loop — and is reported with GaveUp + ErrGaveUp.
func TestPermanentFailureHitsCap(t *testing.T) {
	clk := &fakeClock{}
	const cap = 3
	s := New(clk, Policy{MaxRestarts: cap, BaseBackoff: 5 * time.Millisecond, Factor: 2})
	c := &permaComponent{name: "hotcore"}

	wait := superviseAsync(s, context.Background(), c)

	// It will request `cap` backoffs (one per restart); release each.
	for i := 0; i < cap; i++ {
		waitForPending(t, clk, 1)
		clk.release()
	}

	out, err := wait()
	if err == nil {
		t.Fatalf("expected give-up error, got nil (outcome %+v)", out)
	}
	if !errors.Is(err, ErrGaveUp) {
		t.Fatalf("expected ErrGaveUp, got %v", err)
	}
	if !out.GaveUp {
		t.Fatalf("expected GaveUp=true, got %+v", out)
	}
	if out.Restarts != cap {
		t.Fatalf("expected exactly %d restarts (bounded), got %d", cap, out.Restarts)
	}
	// Initial run + cap restarts = cap+1 total Run calls. Bounded, not a storm.
	if c.callCount() != cap+1 {
		t.Fatalf("expected %d Run calls (bounded), got %d", cap+1, c.callCount())
	}
}

// TestBackoffGrowsGeometrically asserts the injected clock saw growing backoffs:
// Base, Base*Factor, Base*Factor^2 ... clamped to MaxBackoff.
func TestBackoffGrowsGeometrically(t *testing.T) {
	clk := &fakeClock{}
	s := New(clk, Policy{
		MaxRestarts: 4,
		BaseBackoff: 10 * time.Millisecond,
		MaxBackoff:  100 * time.Millisecond,
		Factor:      2,
	})
	c := &permaComponent{name: "channel"}

	wait := superviseAsync(s, context.Background(), c)
	for i := 0; i < 4; i++ {
		waitForPending(t, clk, 1)
		clk.release()
	}
	out, _ := wait()

	got := clk.durations()
	want := []time.Duration{
		10 * time.Millisecond, // attempt 1: Base
		20 * time.Millisecond, // attempt 2: Base*2
		40 * time.Millisecond, // attempt 3: Base*4
		80 * time.Millisecond, // attempt 4: Base*8 (< MaxBackoff 100ms)
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d backoff requests, got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff[%d] = %v, want %v (full: %v)", i, got[i], want[i], got)
		}
	}
	// Outcome records the same backoffs it applied.
	if len(out.Backoffs) != len(want) {
		t.Fatalf("outcome backoffs = %v, want %v", out.Backoffs, want)
	}
}

// TestBackoffClampedToMax: once geometric growth exceeds MaxBackoff it stays pinned.
func TestBackoffClampedToMax(t *testing.T) {
	p := Policy{BaseBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, Factor: 2}.normalize()
	// attempts: 10, 20, 40, then clamp at 50, 50, ...
	want := []time.Duration{10, 20, 40, 50, 50}
	for i, w := range want {
		got := p.backoffFor(i + 1)
		if got != w*time.Millisecond {
			t.Fatalf("backoffFor(%d) = %v, want %v", i+1, got, w*time.Millisecond)
		}
	}
}

// TestContextCancelStopsSupervision: cancelling the context during a backoff wait
// stops the supervisor promptly without exhausting the cap.
func TestContextCancelStopsSupervision(t *testing.T) {
	clk := &fakeClock{}
	s := New(clk, Policy{MaxRestarts: 100, BaseBackoff: time.Second, Factor: 2})
	c := &permaComponent{name: "feed"}

	ctx, cancel := context.WithCancel(context.Background())
	wait := superviseAsync(s, ctx, c)

	waitForPending(t, clk, 1) // supervisor is waiting on the first backoff
	cancel()                  // cancel instead of releasing the timer

	out, err := wait()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if out.GaveUp {
		t.Fatalf("cancel should not be a give-up: %+v", out)
	}
	if c.callCount() != 1 {
		t.Fatalf("expected 1 Run call before cancel, got %d", c.callCount())
	}
}

// TestEventsEmitted verifies lifecycle events fire in order for a flaky component.
func TestEventsEmitted(t *testing.T) {
	clk := &fakeClock{}
	s := New(clk, Policy{MaxRestarts: 3, BaseBackoff: time.Millisecond, Factor: 2})

	var (
		mu  sync.Mutex
		got []EventKind
	)
	s.OnEvent(func(e Event) {
		mu.Lock()
		got = append(got, e.Kind)
		mu.Unlock()
	})

	c := &flakyComponent{name: "x", failTimes: 1}
	wait := superviseAsync(s, context.Background(), c)
	waitForPending(t, clk, 1)
	clk.release()
	if _, err := wait(); err != nil {
		t.Fatalf("recover: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// Expect: Failed, Restarting, Recovered.
	want := []EventKind{EventFailed, EventRestarting, EventRecovered}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestSuperviseAllConcurrent runs a recoverer and a permanent failer together and
// checks both outcomes are reported (bounded).
func TestSuperviseAllConcurrent(t *testing.T) {
	clk := &fakeClock{}
	s := New(clk, Policy{MaxRestarts: 2, BaseBackoff: time.Millisecond, Factor: 2})
	ok := &flakyComponent{name: "ok", failTimes: 1}
	bad := &permaComponent{name: "bad"}

	done := make(chan map[string]Outcome, 1)
	go func() { done <- s.SuperviseAll(context.Background(), ok, bad) }()

	// Drain timers until both components finish. Each restart requests one timer.
	deadline := time.Now().Add(3 * time.Second)
	var results map[string]Outcome
	for {
		select {
		case results = <-done:
		default:
		}
		if results != nil {
			break
		}
		if clk.pendingCount() > 0 {
			clk.release()
		} else {
			time.Sleep(time.Millisecond)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for SuperviseAll")
		}
	}

	if !results["ok"].Recovered {
		t.Fatalf("ok should recover: %+v", results["ok"])
	}
	if !results["bad"].GaveUp {
		t.Fatalf("bad should give up: %+v", results["bad"])
	}
	if results["bad"].Restarts != 2 {
		t.Fatalf("bad restarts bounded to 2, got %d", results["bad"].Restarts)
	}
}
