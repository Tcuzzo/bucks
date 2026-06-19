package channel

import (
	"context"
	"sync"
	"time"
)

// quiethours.go enforces the operator's REST rule on the outbound operator channel:
// during quiet hours (default 01:00–06:00 local) BUCKS does NOT ping or chatter. A
// ROUTINE alert or a scheduled report is HELD; a real EMERGENCY (an AlertCritical —
// the breach/kill-switch level) breaks through. Operator-initiated, time-sensitive
// traffic — an approval request — is NEVER gated: a trade decision can't wait, and
// the fail-safe approval path already governs silence (a timeout is Denied).
//
// SCOPE OF "HELD" THIS SLICE: holding is DROP-WITH-LOG, not a durable replay queue.
// We satisfy the operator's "held until 6am" by simply NOT pinging during the quiet
// window (no chime, no wake), recording the drop so it is observable, and emitting a
// log line. A replay-at-06:00 queue that re-delivers the held items is a deliberate
// FUTURE enhancement and is intentionally NOT built here — do not mistake QuietChannel
// for a store-and-forward queue.

// QuietHours decides whether a given local time falls inside the operator's quiet
// window [StartHour, EndHour). The window does not cross midnight (the real config,
// 01:00–06:00, has Start < End). The clock is injectable so the gate stays testable
// with no wall clock in the logic.
type QuietHours struct {
	StartHour int
	EndHour   int
	now       func() time.Time
}

// QuietHoursOption configures a QuietHours (and, via NewQuietChannel, the wrapper).
type QuietHoursOption func(*QuietHours)

// WithHours sets the quiet window bounds: quiet is [start, end) by local hour. The
// real operator config is 1..6 (the default). Callers pass Start < End (no midnight
// wrap in this slice).
func WithHours(start, end int) QuietHoursOption {
	return func(q *QuietHours) {
		q.StartHour = start
		q.EndHour = end
	}
}

// WithNow injects the clock used by IsQuiet's no-arg sense (and by QuietChannel to
// stamp "is it quiet right now"). Tests pin it to a fixed instant.
func WithNow(now func() time.Time) QuietHoursOption {
	return func(q *QuietHours) { q.now = now }
}

// NewQuietHours builds a QuietHours defaulting to the operator's 01:00–06:00 local
// window with the real wall clock, then applies any options.
func NewQuietHours(opts ...QuietHoursOption) QuietHours {
	q := QuietHours{StartHour: 1, EndHour: 6, now: time.Now}
	for _, opt := range opts {
		opt(&q)
	}
	if q.now == nil {
		q.now = time.Now
	}
	return q
}

// IsQuiet reports whether t's LOCAL hour is in [StartHour, EndHour). The start hour
// is inclusive (01:00 is quiet) and the end hour is exclusive (06:00 is NOT quiet —
// the operator is waking). Minutes inside the start hour count as quiet (01:30), and
// minutes inside the end hour do NOT (06:30 is not quiet, since hour 6 >= EndHour).
func (q QuietHours) IsQuiet(t time.Time) bool {
	h := t.Local().Hour()
	return h >= q.StartHour && h < q.EndHour
}

// nowIsQuiet evaluates IsQuiet against the injected clock.
func (q QuietHours) nowIsQuiet() bool { return q.IsQuiet(q.now()) }

// QuietChannel wraps an inner Channel and applies the quiet-hours gate to outbound,
// non-time-sensitive traffic (routine alerts and reports). Emergencies and approval
// requests pass straight through. It records how many items it has held (drop-with-
// log; see the package note above — NOT a replay queue) and exposes the inner
// channel so forwarded calls remain observable in tests.
type QuietChannel struct {
	inner Channel
	hours QuietHours

	// logf is the held-item log seam. Defaults to a no-op so tests stay silent; a
	// production caller can wire it to the real logger.
	logf func(format string, args ...any)

	mu   sync.Mutex
	held int
}

// NewQuietChannel wraps inner with the quiet-hours gate. Options configure the
// window and the clock (WithHours / WithNow); with no options it uses the operator's
// 01:00–06:00 local default and the real wall clock.
func NewQuietChannel(inner Channel, opts ...QuietHoursOption) *QuietChannel {
	return &QuietChannel{
		inner: inner,
		hours: NewQuietHours(opts...),
		logf:  func(string, ...any) {}, // no-op by default
	}
}

// WithLogf sets the held-item log seam (chainable). Holding logs a single line per
// dropped item so the operator can later see what was suppressed.
func (c *QuietChannel) WithLogf(logf func(format string, args ...any)) *QuietChannel {
	if logf != nil {
		c.logf = logf
	}
	return c
}

// Inner returns the wrapped channel so tests (and callers) can observe forwarded
// traffic directly.
func (c *QuietChannel) Inner() Channel { return c.inner }

// Held returns how many items have been held (dropped-with-log) by the quiet-hours
// gate so far.
func (c *QuietChannel) Held() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held
}

// isEmergency reports whether an alert level must break through quiet hours. Only the
// breach / kill-switch level (AlertCritical) qualifies — Info and Warn are routine.
func isEmergency(level AlertLevel) bool { return level == AlertCritical }

// hold records one suppressed item and logs it. Returns nil so the caller treats the
// (silently held) send as a non-error: we deliberately did not ping the operator.
func (c *QuietChannel) hold(kind, detail string) error {
	c.mu.Lock()
	c.held++
	c.mu.Unlock()
	c.logf("quiet-hours: held %s (%s) — not pinging operator (drop-with-log, no replay queue)", kind, detail)
	return nil
}

// SendAlert forwards the alert unless it is routine AND we are in quiet hours, in
// which case it is HELD (no inner call, recorded, nil returned). An emergency
// (AlertCritical) always forwards, even during quiet hours.
func (c *QuietChannel) SendAlert(ctx context.Context, a Alert) error {
	if c.hours.nowIsQuiet() && !isEmergency(a.Level) {
		return c.hold("alert", a.Level.String())
	}
	return c.inner.SendAlert(ctx, a)
}

// SendReport forwards the report unless we are in quiet hours, in which case it is
// HELD (reports are always routine — there is no emergency report).
func (c *QuietChannel) SendReport(ctx context.Context, r Report) error {
	if c.hours.nowIsQuiet() {
		return c.hold("report", "scheduled")
	}
	return c.inner.SendReport(ctx, r)
}

// RequestApproval ALWAYS forwards to the inner channel — never gated by quiet hours.
// A trade decision is time-sensitive, and the fail-safe approval path (a timeout is
// Denied) already governs silence; suppressing the ask would silently turn an
// above-band trade into a no with no operator visibility, which the operator-
// authority law forbids.
func (c *QuietChannel) RequestApproval(ctx context.Context, r ApprovalRequest) (Decision, error) {
	return c.inner.RequestApproval(ctx, r)
}

// compile-time assertion that QuietChannel satisfies the operator channel boundary.
var _ Channel = (*QuietChannel)(nil)
