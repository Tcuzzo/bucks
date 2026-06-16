// Package supervisor is BUCKS's self-heal layer: it keeps the trader's moving parts
// alive (the Rust hot-core, the data feed, the channel) by restarting a failed
// Component with BOUNDED backoff and a hard max-restart cap. A permanently-broken
// component is reported and given up on — never an unbounded restart storm. The
// clock is injectable so backoff growth is tested deterministically (no real sleeps
// in the suite, the Hydra "tests must not stall" lesson applied).
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Component is a self-healing unit. Run blocks until the component exits: a nil
// return means a clean stop (no restart); a non-nil error (or ctx cancellation
// surfaced as an error) means it failed and the supervisor will consider a restart.
// Run MUST honor ctx and return promptly when it is cancelled.
type Component interface {
	Name() string
	Run(ctx context.Context) error
}

// Clock is the time source the supervisor uses for backoff. Real runtime uses
// RealClock; tests inject a FakeClock to advance time deterministically.
type Clock interface {
	// After returns a channel that delivers once the duration has elapsed.
	After(d time.Duration) <-chan time.Time
}

// RealClock is the production Clock backed by time.After.
type RealClock struct{}

// After implements Clock using the real wall clock.
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Policy bounds restart behavior. Zero values are filled with safe defaults by
// normalize, so a Policy{} is usable.
type Policy struct {
	// MaxRestarts is the hard cap on restarts after the first failure. After this
	// many restarts the component is given up on and reported. Must be >= 0.
	MaxRestarts int
	// BaseBackoff is the first restart delay; it grows geometrically.
	BaseBackoff time.Duration
	// MaxBackoff caps the geometric growth so the delay never runs away.
	MaxBackoff time.Duration
	// Factor multiplies the backoff each restart (>= 1). Default 2 (exponential).
	Factor int
}

func (p Policy) normalize() Policy {
	if p.BaseBackoff <= 0 {
		p.BaseBackoff = 100 * time.Millisecond
	}
	if p.MaxBackoff <= 0 {
		p.MaxBackoff = 30 * time.Second
	}
	if p.Factor < 1 {
		p.Factor = 2
	}
	if p.MaxRestarts < 0 {
		p.MaxRestarts = 0
	}
	return p
}

// backoffFor returns the delay before restart attempt n (n starts at 1 for the
// first restart). It grows geometrically: Base, Base*Factor, Base*Factor^2 ...,
// clamped to MaxBackoff. n <= 0 yields 0 (no wait).
func (p Policy) backoffFor(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	d := p.BaseBackoff
	for i := 1; i < n; i++ {
		d *= time.Duration(p.Factor)
		if d >= p.MaxBackoff || d <= 0 { // <=0 guards overflow
			return p.MaxBackoff
		}
	}
	if d > p.MaxBackoff {
		return p.MaxBackoff
	}
	return d
}

// Outcome records how supervising a component ended.
type Outcome struct {
	Name      string          // component name
	Restarts  int             // number of restarts performed
	Recovered bool            // true if it ran clean (nil) after restart(s)
	GaveUp    bool            // true if the max-restart cap was hit
	Backoffs  []time.Duration // the backoff applied before each restart (in order)
	LastErr   error           // the failure that ended supervision (nil if recovered)
}

// ErrGaveUp wraps the final failure when a component exhausts its restart cap.
var ErrGaveUp = errors.New("supervisor: max restarts exceeded")

// Supervisor runs and self-heals Components. The zero value is not usable; use New.
type Supervisor struct {
	clock  Clock
	policy Policy

	mu      sync.Mutex
	onEvent func(Event) // optional observer, called under no lock
}

// Event is emitted on each lifecycle transition so callers (and tests) can observe
// restarts and give-ups without polling.
type Event struct {
	Name    string
	Kind    EventKind
	Attempt int           // restart attempt number for Restarting events
	Backoff time.Duration // applied backoff for Restarting events
	Err     error         // failure that triggered the event (if any)
}

// EventKind enumerates supervisor lifecycle events.
type EventKind int

const (
	// EventFailed: a component's Run returned an error.
	EventFailed EventKind = iota
	// EventRestarting: the supervisor is waiting backoff then restarting.
	EventRestarting
	// EventRecovered: the component ran clean (nil) after a failure.
	EventRecovered
	// EventGaveUp: the restart cap was hit; the component is abandoned.
	EventGaveUp
)

func (k EventKind) String() string {
	switch k {
	case EventFailed:
		return "failed"
	case EventRestarting:
		return "restarting"
	case EventRecovered:
		return "recovered"
	case EventGaveUp:
		return "gaveup"
	default:
		return "unknown"
	}
}

// New builds a Supervisor with the given clock and policy. A nil clock defaults to
// RealClock; the policy is normalized so a zero Policy is valid.
func New(clock Clock, policy Policy) *Supervisor {
	if clock == nil {
		clock = RealClock{}
	}
	return &Supervisor{clock: clock, policy: policy.normalize()}
}

// OnEvent registers an observer for lifecycle events. Pass nil to clear.
func (s *Supervisor) OnEvent(fn func(Event)) {
	s.mu.Lock()
	s.onEvent = fn
	s.mu.Unlock()
}

func (s *Supervisor) emit(e Event) {
	s.mu.Lock()
	fn := s.onEvent
	s.mu.Unlock()
	if fn != nil {
		fn(e)
	}
}

// Supervise runs c, restarting it on failure with bounded backoff up to the policy
// cap. It returns when the component recovers (ran clean), the cap is exhausted, or
// ctx is cancelled. It NEVER loops unbounded: at most policy.MaxRestarts restarts.
//
// Semantics:
//   - Run returns nil  -> clean stop, Outcome.Recovered=true, done.
//   - Run returns error -> failure; if restarts remain, wait backoff then re-Run;
//     else Outcome.GaveUp=true, LastErr set, done.
//   - ctx cancelled while waiting backoff -> stop early, return ctx.Err().
func (s *Supervisor) Supervise(ctx context.Context, c Component) (Outcome, error) {
	name := c.Name()
	out := Outcome{Name: name}

	for {
		err := c.Run(ctx)
		if err == nil {
			out.Recovered = true
			s.emit(Event{Name: name, Kind: EventRecovered})
			return out, nil
		}
		out.LastErr = err
		s.emit(Event{Name: name, Kind: EventFailed, Err: err})

		// If ctx is already done, surface that rather than restarting.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return out, ctxErr
		}

		if out.Restarts >= s.policy.MaxRestarts {
			out.GaveUp = true
			s.emit(Event{Name: name, Kind: EventGaveUp, Err: err})
			return out, fmt.Errorf("%w: %s after %d restarts: %v",
				ErrGaveUp, name, out.Restarts, err)
		}

		attempt := out.Restarts + 1
		backoff := s.policy.backoffFor(attempt)
		out.Backoffs = append(out.Backoffs, backoff)
		s.emit(Event{Name: name, Kind: EventRestarting, Attempt: attempt, Backoff: backoff, Err: err})

		if backoff > 0 {
			select {
			case <-ctx.Done():
				return out, ctx.Err()
			case <-s.clock.After(backoff):
			}
		} else {
			// Even with zero backoff, respect cancellation between attempts.
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
		}
		out.Restarts = attempt
	}
}

// SuperviseAll supervises every component concurrently and returns their outcomes
// keyed by name once all have finished (recovered or given up). It is the entry the
// harness uses to keep the hot-core, data feed and channel alive together.
func (s *Supervisor) SuperviseAll(ctx context.Context, comps ...Component) map[string]Outcome {
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = make(map[string]Outcome, len(comps))
	)
	for _, c := range comps {
		wg.Add(1)
		go func(c Component) {
			defer wg.Done()
			out, _ := s.Supervise(ctx, c)
			mu.Lock()
			results[c.Name()] = out
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	return results
}
