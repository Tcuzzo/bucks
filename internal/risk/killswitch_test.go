package risk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeClock is a controllable clock for deterministic tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{t: start} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestKillSwitch_HaltPersistsAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "killswitch.json")
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())

	ks, err := Open(path, WithClock(clk.now))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if halted, _ := ks.IsHalted(); halted {
		t.Fatal("fresh switch must not be halted")
	}

	if err := ks.Halt("daily loss budget reached", HaltMaxDailyLoss); err != nil {
		t.Fatalf("halt: %v", err)
	}
	halted, reason := ks.IsHalted()
	if !halted {
		t.Fatal("switch must be halted after Halt")
	}
	if reason != "daily loss budget reached" {
		t.Fatalf("reason not preserved: %q", reason)
	}
	if ks.Kind() != HaltMaxDailyLoss {
		t.Fatalf("kind not preserved: %s", ks.Kind())
	}

	// The file must exist on disk (durable).
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("durable file missing: %v", err)
	}

	// Simulate a RESTART: open a brand-new KillSwitch on the same file. It MUST
	// come up HALTED (never auto-resume into a breach).
	ks2, err := Open(path, WithClock(clk.now))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	halted2, reason2 := ks2.IsHalted()
	if !halted2 {
		t.Fatal("RESTART must come up HALTED — auto-resume into a breach is forbidden")
	}
	if reason2 != "daily loss budget reached" {
		t.Fatalf("restart lost the reason: %q", reason2)
	}
	if ks2.Kind() != HaltMaxDailyLoss {
		t.Fatalf("restart lost the kind: %s", ks2.Kind())
	}
}

func TestKillSwitch_ClearIsExplicitOnlyAndNoAutoResume(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "killswitch.json")
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())

	ks, err := Open(path, WithClock(clk.now))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := ks.Halt("manual stop", HaltManual); err != nil {
		t.Fatalf("halt: %v", err)
	}

	// Advancing time alone must NOT clear the halt — there is no auto-resume.
	clk.advance(72 * time.Hour)
	if halted, _ := ks.IsHalted(); !halted {
		t.Fatal("time passing must NOT auto-clear a halt")
	}

	// Reopening after a long time must still be HALTED.
	ksReboot, err := Open(path, WithClock(clk.now))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if halted, _ := ksReboot.IsHalted(); !halted {
		t.Fatal("reopen after long delay must still be HALTED (no time-based resume)")
	}

	// Only an EXPLICIT Clear() lifts it.
	if err := ksReboot.Clear(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if halted, _ := ksReboot.IsHalted(); halted {
		t.Fatal("after explicit Clear, switch must be un-halted")
	}

	// The clear is durable: a fresh open comes up un-halted.
	ksAfterClear, err := Open(path, WithClock(clk.now))
	if err != nil {
		t.Fatalf("reopen after clear: %v", err)
	}
	if halted, _ := ksAfterClear.IsHalted(); halted {
		t.Fatal("cleared state must persist across restart")
	}
}

func TestKillSwitch_RunawayGuardHaltsPastThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "killswitch.json")
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())

	// Window 1s, limit 3 orders: the 4th order within the window trips the guard.
	ks, err := Open(path,
		WithClock(clk.now),
		WithRunawayGuard(time.Second, 3),
	)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// 3 orders within the window — under the limit, no halt.
	for i := 0; i < 3; i++ {
		tripped, err := ks.RecordOrder()
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if tripped {
			t.Fatalf("order %d must not trip (limit 3)", i+1)
		}
		clk.advance(100 * time.Millisecond)
	}
	if halted, _ := ks.IsHalted(); halted {
		t.Fatal("3 orders within window must NOT halt")
	}

	// 4th order within the same 1s window -> trips.
	tripped, err := ks.RecordOrder()
	if err != nil {
		t.Fatalf("record 4th: %v", err)
	}
	if !tripped {
		t.Fatal("4th order within window must trip the runaway guard")
	}
	halted, reason := ks.IsHalted()
	if !halted {
		t.Fatal("runaway guard must HALT after threshold")
	}
	if ks.Kind() != HaltRunawayOrderGuard {
		t.Fatalf("wrong halt kind: %s reason=%s", ks.Kind(), reason)
	}

	// And it persisted: a restart comes up halted.
	ks2, err := Open(path, WithClock(clk.now))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if halted2, _ := ks2.IsHalted(); !halted2 {
		t.Fatal("runaway halt must survive restart")
	}
}

func TestKillSwitch_RunawayGuardWindowPrunes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "killswitch.json")
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())

	ks, err := Open(path,
		WithClock(clk.now),
		WithRunawayGuard(time.Second, 3),
	)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Space orders 400ms apart: at any moment at most 3 fall in a 1s window, so
	// the guard never trips even across many orders.
	for i := 0; i < 10; i++ {
		tripped, err := ks.RecordOrder()
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if tripped {
			t.Fatalf("spaced orders must not trip (got trip at %d)", i)
		}
		clk.advance(400 * time.Millisecond)
	}
	if halted, _ := ks.IsHalted(); halted {
		t.Fatal("well-spaced orders must not halt — window pruning failed")
	}
}

func TestKillSwitch_HaltIdempotentPreservesFirstCause(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "killswitch.json")
	ks, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := ks.Halt("first cause", HaltMaxDailyLoss); err != nil {
		t.Fatalf("halt1: %v", err)
	}
	if err := ks.Halt("second cause", HaltManual); err != nil {
		t.Fatalf("halt2: %v", err)
	}
	_, reason := ks.IsHalted()
	if reason != "first cause" {
		t.Fatalf("first cause must be preserved, got %q", reason)
	}
	if ks.Kind() != HaltMaxDailyLoss {
		t.Fatalf("first kind must be preserved, got %s", ks.Kind())
	}
}

func TestKillSwitch_CorruptFileFailsSafeHalted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "killswitch.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	ks, err := Open(path)
	if err != nil {
		t.Fatalf("open should not hard-error on corrupt file: %v", err)
	}
	halted, reason := ks.IsHalted()
	if !halted {
		t.Fatal("corrupt kill-switch file must FAIL SAFE (come up HALTED)")
	}
	if reason == "" {
		t.Fatal("fail-safe halt must carry a reason")
	}
}

// mockBrokerKillSwitch is the test implementation of the broker-native second
// layer — it proves the BrokerKillSwitch seam wires through IsHalted.
type mockBrokerKillSwitch struct {
	mu     sync.Mutex
	halted bool
	reason string
	halts  int
}

func (m *mockBrokerKillSwitch) Halt(_ context.Context, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.halted = true
	m.reason = reason
	m.halts++
	return nil
}

func (m *mockBrokerKillSwitch) IsHalted(_ context.Context) (bool, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.halted, m.reason, nil
}

func TestKillSwitch_BrokerNativeSecondLayer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "killswitch.json")
	broker := &mockBrokerKillSwitch{}
	ks, err := Open(path, WithBrokerKillSwitch(broker))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// App-side not halted, broker not halted -> not halted.
	if halted, _ := ks.IsHalted(); halted {
		t.Fatal("nothing halted yet")
	}
	// Broker asserts its native halt -> IsHalted must report halted via the seam.
	if err := broker.Halt(context.Background(), "exchange circuit breaker"); err != nil {
		t.Fatalf("broker halt: %v", err)
	}
	halted, reason := ks.IsHalted()
	if !halted {
		t.Fatal("broker-native halt must surface through the app-side IsHalted")
	}
	if reason != "broker-native: exchange circuit breaker" {
		t.Fatalf("broker reason not surfaced: %q", reason)
	}
}

// errSyncer is a syncer whose Sync always fails, used to exercise the durable-
// write failure path (the fsync seam in persistStateLocked).
type errSyncer struct{ err error }

func (e errSyncer) Sync() error { return e.err }

// TestKillSwitch_HaltPersistFailsDoesNotCommit is the regression for R1
// (persist-before-commit): if the durable write fails during Halt, Halt must
// return the error AND the in-memory flag must stay un-halted, so memory never
// claims HALTED while disk does not (a restart would silently lose that halt).
//
// This test FAILS under the OLD ordering (set k.state THEN persist: the flag is
// already flipped, so IsHalted() reports true even though persist errored) and
// PASSES under the fix (persist THEN commit: a failed persist leaves the flag
// untouched).
func TestKillSwitch_HaltPersistFailsDoesNotCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "killswitch.json")
	wantErr := errors.New("injected fsync failure")

	ks, err := Open(path, WithSyncer(func(*os.File) syncer { return errSyncer{err: wantErr} }))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Halt must surface the durable-write error.
	gotErr := ks.Halt("daily loss budget reached", HaltMaxDailyLoss)
	if gotErr == nil {
		t.Fatal("Halt must return the durable-write error when persist fails")
	}
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("Halt error must wrap the injected fsync failure, got %v", gotErr)
	}

	// The pre-commit state MUST be preserved: a failed persist leaves us un-halted.
	if halted, reason := ks.IsHalted(); halted {
		t.Fatalf("persist-before-commit: a failed durable write must leave the switch un-halted, got halted=true reason=%q", reason)
	}
	if ks.Kind() != HaltNone {
		t.Fatalf("kind must stay HaltNone after a failed-persist Halt, got %s", ks.Kind())
	}

	// And nothing must have been left on disk claiming a halt: a fresh open comes
	// up un-halted (the atomic temp-file write never reached the real path).
	ks2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if halted, _ := ks2.IsHalted(); halted {
		t.Fatal("failed persist must not have written a halted state to disk")
	}
}
