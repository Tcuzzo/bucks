package data

import (
	"context"
	"fmt"
)

// ErrNotLiveSafe is returned by AssertLiveTradePath when a non-live-safe source is
// offered to the live trade path. The error names the offending source so the
// failure is actionable, not a mystery.
type ErrNotLiveSafe struct {
	Source string
}

func (e *ErrNotLiveSafe) Error() string {
	return fmt.Sprintf("data: source %q is NOT live-safe (delayed/backfill feed) and is banned from the live trade path", e.Source)
}

// Named is implemented by sources that can report a human name for diagnostics and
// for the live-safe error message. Sources in this package implement it.
type Named interface {
	Name() string
}

// AssertLiveTradePath REFUSES a non-live-safe source on the LIVE TRADE path. It is
// the enforcement point for build spec §4.6: "yfinance / Alpha Vantage / free-
// Polygon are backfill/backtest only — BANNED from the live trigger path." Trading
// on delayed data is the #1 silent killer, so this is a hard gate, not a warning.
//
// It returns nil for a live-safe source and *ErrNotLiveSafe (naming the source)
// for a non-live-safe one. Wire it on the path that arms the live trigger: a
// backfill feed can never be silently substituted for a real-time one.
func AssertLiveTradePath(src DataSource) error {
	if src.LiveSafe() {
		return nil
	}
	return &ErrNotLiveSafe{Source: sourceName(src)}
}

// sourceName returns a source's Name() if it implements Named, else a generic tag.
func sourceName(src DataSource) string {
	if n, ok := src.(Named); ok {
		return n.Name()
	}
	return fmt.Sprintf("%T", src)
}

// RealtimeSource is an exchange-WebSocket / Alpaca-style real-time feed. It is
// LIVE-SAFE: it carries real-time data and is permitted on the live trade path. It
// is fed by an in-process channel here (so it is fully testable with a fake stream
// — NO live network), which a production feed goroutine would write from its socket.
type RealtimeSource struct {
	name   string
	frames chan Frame
}

// NewRealtimeSource builds a real-time source with the given name. The returned
// channel is written by the caller (or a network read loop) and closed to end the
// stream. Use Emit/CloseStream in tests.
func NewRealtimeSource(name string, buffer int) *RealtimeSource {
	return &RealtimeSource{name: name, frames: make(chan Frame, buffer)}
}

// Subscribe begins streaming for symbols. The in-process source needs no network
// setup; it returns nil. (A real exchange source would open its WebSocket here.)
func (s *RealtimeSource) Subscribe(ctx context.Context, symbols []string) error {
	_ = ctx
	_ = symbols
	return nil
}

// Frames returns the receive-only frame stream.
func (s *RealtimeSource) Frames() <-chan Frame { return s.frames }

// LiveSafe reports true: a real-time feed is permitted on the live trade path.
func (s *RealtimeSource) LiveSafe() bool { return true }

// Name returns the source name for diagnostics and gating errors.
func (s *RealtimeSource) Name() string { return s.name }

// Emit pushes a frame onto the stream (the producer side of the single-owner
// handoff). Blocks if the buffer is full, mirroring real backpressure.
func (s *RealtimeSource) Emit(f Frame) { s.frames <- f }

// CloseStream closes the frame channel, signaling end-of-stream to the Ingestor.
// Separate from Close so a test can end the stream then assert a clean drain.
func (s *RealtimeSource) CloseStream() { close(s.frames) }

// Close stops the source. For the in-process source it closes the stream if it is
// still open; it is safe to call once. (Production would also tear down the socket.)
func (s *RealtimeSource) Close() error {
	defer func() { _ = recover() }() // closing an already-closed channel is fine to ignore on shutdown
	close(s.frames)
	return nil
}

// BackfillSource is a yfinance / Alpha Vantage-style DELAYED/historical feed. It is
// NOT live-safe: its data is delayed and it is banned from the live trade path. It
// is legitimate for BACKTEST/backfill, where AssertLiveTradePath is never invoked.
type BackfillSource struct {
	name   string
	frames chan Frame
}

// NewBackfillSource builds a backfill source with the given name.
func NewBackfillSource(name string, buffer int) *BackfillSource {
	return &BackfillSource{name: name, frames: make(chan Frame, buffer)}
}

// Subscribe begins streaming for symbols. Returns nil (no network in this stub).
func (s *BackfillSource) Subscribe(ctx context.Context, symbols []string) error {
	_ = ctx
	_ = symbols
	return nil
}

// Frames returns the receive-only frame stream.
func (s *BackfillSource) Frames() <-chan Frame { return s.frames }

// LiveSafe reports FALSE: delayed/backfill data must never arm the live trigger.
func (s *BackfillSource) LiveSafe() bool { return false }

// Name returns the source name for diagnostics and gating errors.
func (s *BackfillSource) Name() string { return s.name }

// Emit pushes a frame onto the (backtest-only) stream.
func (s *BackfillSource) Emit(f Frame) { s.frames <- f }

// CloseStream closes the frame channel.
func (s *BackfillSource) CloseStream() { close(s.frames) }

// Close stops the source.
func (s *BackfillSource) Close() error {
	defer func() { _ = recover() }()
	close(s.frames)
	return nil
}
