package risk

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// HaltKind labels what tripped the kill switch. It is persisted with the halt so
// an operator (and the post-restart boot) can see WHY trading is halted.
type HaltKind int

const (
	// HaltNone is the zero value: not halted.
	HaltNone HaltKind = iota
	// HaltMaxDailyLoss is a halt triggered by the daily-loss circuit breaker.
	HaltMaxDailyLoss
	// HaltRunawayOrderGuard is a halt triggered by too many orders in a window
	// (a runaway/looping strategy guard).
	HaltRunawayOrderGuard
	// HaltManual is an operator-initiated halt.
	HaltManual
	// HaltBrokerNative is a halt asserted by the broker-native kill switch layer.
	HaltBrokerNative
)

// String renders the halt kind for logs, the durable file, and tests.
func (k HaltKind) String() string {
	switch k {
	case HaltNone:
		return "None"
	case HaltMaxDailyLoss:
		return "MaxDailyLoss"
	case HaltRunawayOrderGuard:
		return "RunawayOrderGuard"
	case HaltManual:
		return "Manual"
	case HaltBrokerNative:
		return "BrokerNative"
	default:
		return fmt.Sprintf("HaltKind(%d)", int(k))
	}
}

// haltState is the on-disk JSON shape. It is intentionally tiny and stable: a
// future version can add fields without breaking the "is it halted" read.
type haltState struct {
	Halted   bool      `json:"halted"`
	Kind     HaltKind  `json:"kind"`
	Reason   string    `json:"reason"`
	HaltedAt time.Time `json:"halted_at"`
}

// syncer is the fsync seam (mirrors orders.Journal): *os.File satisfies it via
// Sync(). Production code fsyncs the real file; tests can route through a fake to
// observe durability without depending on real disk semantics.
type syncer interface {
	Sync() error
}

// KillSwitch is the app-side, durable, fail-safe HALTED flag — the first of the
// two kill-switch layers (the second is broker-native, via BrokerKillSwitch).
//
// Durability + fail-safe semantics:
//   - Halt persists the HALTED state to disk (atomic write + fsync) BEFORE
//     returning, so it survives a crash/restart.
//   - Open reads the file on construction: if it says HALTED, the switch comes up
//     HALTED. It NEVER auto-resumes into a breach after a restart.
//   - Clear() is the ONLY way out of a halt, and it requires an explicit call
//     (an operator action). There is no timer, no auto-clear, no "resume after N
//     minutes." A breach stays halted until a human clears it.
//
// All state is mutex-guarded; the file writer is atomic (temp file + rename) and
// fsync'd, so a concurrent reader never sees a half-written file.
type KillSwitch struct {
	mu    sync.Mutex
	path  string
	state haltState

	// runaway-order guard (clock-injected window counter).
	//
	// maxOrdersPerWindow is the maximum number of orders allowed WITHIN
	// runawayWindow. The guard trips on the NEXT order over this many: i.e. once
	// the count in the window would exceed maxOrdersPerWindow (count >
	// maxOrdersPerWindow), the switch halts. So with maxOrdersPerWindow=3 the 4th
	// order inside the window is the one that trips. A non-positive value (or a
	// non-positive window) disables the guard.
	now                func() time.Time
	runawayWindow      time.Duration
	maxOrdersPerWindow int
	orderTimes         []time.Time

	// broker-native second layer (optional). When set, IsHalted also consults it.
	brokerLayer BrokerKillSwitch

	// newFile/openFile are test seams for the durable writer. In production they
	// wrap os; tests can override to inject a syncer or force write errors.
	openSyncer func(*os.File) syncer
}

// BrokerKillSwitch is the typed hook for the SECOND kill-switch layer: the
// broker/exchange-native kill switch (e.g. Alpaca's account-level trading halt,
// or an exchange "cancel-all + block" endpoint). The broker adapter implements
// this; the app-side KillSwitch consults it but does NOT call live venue APIs
// here — wiring the live call is the adapter's job and out of scope for this
// package. The mock implementation in tests proves the seam.
//
// Halt asks the broker to assert its native halt; Cleared reports whether the
// broker-native layer currently considers trading halted.
type BrokerKillSwitch interface {
	// Halt asks the broker to assert its native trading halt for the given reason.
	Halt(ctx context.Context, reason string) error
	// IsHalted reports whether the broker-native layer currently halts trading.
	IsHalted(ctx context.Context) (bool, string, error)
}

// KillSwitchOption configures a KillSwitch at Open time.
type KillSwitchOption func(*KillSwitch)

// WithClock injects the clock used by the runaway-order guard (and halt
// timestamps). Tests pass a controllable clock so behavior is deterministic.
func WithClock(now func() time.Time) KillSwitchOption {
	return func(k *KillSwitch) {
		if now != nil {
			k.now = now
		}
	}
}

// WithRunawayGuard configures the runaway-order guard. maxOrdersPerWindow is the
// maximum number of orders permitted within window; the guard halts on the NEXT
// order over that many (count > maxOrdersPerWindow). So WithRunawayGuard(1s, 3)
// permits 3 orders per second and trips on the 4th within the window. A
// non-positive maxOrdersPerWindow or window disables the guard.
func WithRunawayGuard(window time.Duration, maxOrdersPerWindow int) KillSwitchOption {
	return func(k *KillSwitch) {
		k.runawayWindow = window
		k.maxOrdersPerWindow = maxOrdersPerWindow
	}
}

// WithBrokerKillSwitch registers the broker-native second layer.
func WithBrokerKillSwitch(b BrokerKillSwitch) KillSwitchOption {
	return func(k *KillSwitch) { k.brokerLayer = b }
}

// WithSyncer overrides the durable-write fsync seam. In production the default
// wraps the real *os.File (its Sync fsyncs to disk); tests inject a syncer that
// returns an error to exercise the persist-before-commit failure path (a failed
// durable write must NOT flip the in-memory HALTED flag). A nil wrap is ignored.
func WithSyncer(wrap func(*os.File) syncer) KillSwitchOption {
	return func(k *KillSwitch) {
		if wrap != nil {
			k.openSyncer = wrap
		}
	}
}

// Open constructs a KillSwitch backed by the durable file at path. If the file
// exists and says HALTED, the switch comes up HALTED (never auto-resumes into a
// breach). A missing file means "not halted" (fresh start). A corrupt/unreadable
// file is treated FAIL-SAFE: the switch comes up HALTED with a Manual reason so a
// human inspects it rather than the trader resuming blindly.
func Open(path string, opts ...KillSwitchOption) (*KillSwitch, error) {
	k := &KillSwitch{
		path:       path,
		now:        time.Now,
		openSyncer: func(f *os.File) syncer { return f },
	}
	for _, opt := range opts {
		opt(k)
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var st haltState
		if jerr := json.Unmarshal(data, &st); jerr != nil {
			// Fail-safe: an unreadable halt file means we cannot prove we are
			// safe to trade, so we come up HALTED and force operator inspection.
			k.state = haltState{
				Halted:   true,
				Kind:     HaltManual,
				Reason:   fmt.Sprintf("kill-switch file unreadable (%v) — fail-safe halt; operator must inspect", jerr),
				HaltedAt: k.now(),
			}
			return k, nil
		}
		k.state = st
	case os.IsNotExist(err):
		k.state = haltState{Halted: false, Kind: HaltNone}
	default:
		// Any other read error (permissions, IO) is also fail-safe HALTED.
		return nil, fmt.Errorf("risk: open kill switch %q: %w", path, err)
	}
	return k, nil
}

// Halt sets the switch to HALTED with the given reason/kind and persists it
// durably (atomic write + fsync) before returning. Halting an already-halted
// switch keeps the ORIGINAL halt (the first cause is preserved) but is not an
// error — repeated triggers are idempotent.
func (k *KillSwitch) Halt(reason string, kind HaltKind) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.state.Halted {
		return nil // already halted; preserve the first cause
	}
	newState := haltState{
		Halted:   true,
		Kind:     kind,
		Reason:   reason,
		HaltedAt: k.now(),
	}
	// Persist-before-commit: the durable file is the source of truth. If the disk
	// write fails we must NOT flip the in-memory flag, otherwise memory would say
	// HALTED while disk does not — a restart would silently lose the halt. Only
	// after persist succeeds do we commit the new state to memory.
	if err := k.persistStateLocked(newState); err != nil {
		return err
	}
	k.state = newState
	return nil
}

// IsHalted reports whether trading is halted and the reason. It returns true if
// EITHER the app-side flag is set OR (when registered) the broker-native layer
// reports halted. The broker layer is consulted with a short-lived context the
// caller controls; here we use a background context since this is a fast local
// check in tests/mocks (the live adapter governs its own timeouts).
func (k *KillSwitch) IsHalted() (bool, string) {
	k.mu.Lock()
	halted := k.state.Halted
	reason := k.state.Reason
	broker := k.brokerLayer
	k.mu.Unlock()

	if halted {
		return true, reason
	}
	if broker != nil {
		if bHalted, bReason, err := broker.IsHalted(context.Background()); err == nil && bHalted {
			return true, "broker-native: " + bReason
		}
	}
	return false, ""
}

// Kind returns the current halt kind (HaltNone when not halted).
func (k *KillSwitch) Kind() HaltKind {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.state.Halted {
		return HaltNone
	}
	return k.state.Kind
}

// Clear lifts the halt. This is the ONLY exit from a halted state and is an
// EXPLICIT operator action: nothing auto-clears. It persists the cleared state
// durably so a subsequent restart also comes up un-halted (the operator's
// decision survives too). Clearing a non-halted switch is a no-op.
func (k *KillSwitch) Clear() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.state.Halted {
		return nil
	}
	newState := haltState{Halted: false, Kind: HaltNone, HaltedAt: k.now()}
	// Persist-before-commit (same invariant as Halt): if the disk write fails we
	// must NOT flip the in-memory flag to cleared, otherwise memory would say
	// CLEARED while disk still says HALTED — a restart would resurrect the halt
	// the operator believed they lifted, or worse, memory would resume trading on
	// an unpersisted clear. Only commit (and reset the runaway window) once the
	// cleared state is durable on disk.
	if err := k.persistStateLocked(newState); err != nil {
		return err
	}
	k.state = newState
	k.orderTimes = nil // reset the runaway window on an explicit clear
	return nil
}

// RecordOrder feeds the runaway-order guard: it timestamps an order send (using
// the injected clock), prunes timestamps outside the window, and if the count in
// the window exceeds maxOrdersPerWindow (i.e. the NEXT order over that many),
// halts the switch with HaltRunawayOrderGuard. It returns true if THIS call
// tripped the guard. A disabled guard (non-positive window/maxOrdersPerWindow)
// records nothing and never trips.
func (k *KillSwitch) RecordOrder() (tripped bool, err error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.maxOrdersPerWindow <= 0 || k.runawayWindow <= 0 {
		return false, nil
	}
	now := k.now()
	// Prune anything older than the window.
	cutoff := now.Add(-k.runawayWindow)
	kept := k.orderTimes[:0]
	for _, t := range k.orderTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	k.orderTimes = append(kept, now)

	if len(k.orderTimes) > k.maxOrdersPerWindow && !k.state.Halted {
		newState := haltState{
			Halted: true,
			Kind:   HaltRunawayOrderGuard,
			Reason: fmt.Sprintf("runaway-order guard: %d orders within %s exceeds limit %d",
				len(k.orderTimes), k.runawayWindow, k.maxOrdersPerWindow),
			HaltedAt: now,
		}
		// Persist-before-commit: only flip the in-memory HALTED flag once the halt
		// is durable on disk, so a failed write never leaves memory halted while
		// disk says clear (which a restart would silently lose). On a write error
		// we still report tripped=true (the guard DID detect a runaway) along with
		// the error, but k.state stays un-halted so the caller's halt-on-error
		// handling drives the response rather than a half-committed flag.
		if perr := k.persistStateLocked(newState); perr != nil {
			return true, perr
		}
		k.state = newState
		return true, nil
	}
	return false, nil
}

// persistStateLocked writes the GIVEN state to disk atomically (temp file in the
// same dir, fsync, rename) so a crash never leaves a half-written file. It does
// NOT mutate k.state — the caller commits to memory only AFTER this returns nil,
// which is what makes Halt/Clear/RecordOrder persist-before-commit (a failed
// write leaves the in-memory flag untouched and returns the error). The caller
// must hold k.mu.
func (k *KillSwitch) persistStateLocked(st haltState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("risk: marshal kill state: %w", err)
	}
	dir := filepath.Dir(k.path)
	tmp, err := os.CreateTemp(dir, ".killswitch-*.tmp")
	if err != nil {
		return fmt.Errorf("risk: temp kill file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("risk: write kill state: %w", err)
	}
	// fsync the data through the (possibly injected) syncer before rename, so the
	// bytes are durable on disk and not just in the page cache.
	if err := k.openSyncer(tmp).Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("risk: fsync kill state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("risk: close kill state: %w", err)
	}
	if err := os.Rename(tmpName, k.path); err != nil {
		return fmt.Errorf("risk: commit kill state: %w", err)
	}
	// fsync the directory so the rename itself is durable.
	if dirF, derr := os.Open(dir); derr == nil {
		_ = k.openSyncer(dirF).Sync()
		_ = dirF.Close()
	}
	return nil
}
