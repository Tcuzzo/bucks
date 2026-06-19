// Package gateway — Gateway is BUCKS's single, always-on Telegram long-poll loop.
// It is the ONLY caller of Telegram getUpdates in the whole program (the single-owner
// principle: the 409 tap-drop lesson — one agent, one bot, one poller). Approvals and
// commands (later slices) plug in behind the Handler seam and consume the updates THIS
// loop dispatches; they do not poll Telegram themselves.
//
// Resilience is the whole point: the loop NEVER exits on a transient fault. A transport
// error, an HTTP 409 Conflict (someone else briefly polled the token), or an HTTP 429
// rate-limit each trigger a jittered back-off and a retry — they do not crash and do not
// return. The only clean exit is the parent context being canceled (shutdown).
//
// Every external seam is injectable so the loop is fully testable without real time or a
// real network: the HTTP client, the clock, the sleep function, the logger, the back-off,
// and the long-poll timeout. The only thing the default wiring leaves untestable is the
// real time.Sleep behind the production sleep func.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"bucks/internal/risk"
)

// defaultPollTimeout is Telegram's long-poll hold time in seconds. 30s is the documented
// sweet spot: long enough to avoid a busy-poll, short enough to notice shutdown quickly.
const defaultPollTimeout = 30

// httpSlack is added to the long-poll timeout to size the per-request HTTP deadline, so
// the HTTP client never cuts off a legitimately-held long-poll before the server replies.
const httpSlack = 10 * time.Second

// Message is the slice of a Telegram message BUCKS cares about: who it is from (chat) and
// what it says (text), plus the message id so a later approval/command slice can edit or
// reply to the exact message the callback came from.
type Message struct {
	MessageID int64 `json:"message_id"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	Text string `json:"text"`
}

// CallbackQuery is an inline-button tap. Data is the button's callback_data; Message is the
// original message the keyboard was attached to (so a handler can answer in place).
type CallbackQuery struct {
	ID      string   `json:"id"`
	Data    string   `json:"data"`
	Message *Message `json:"message"`
}

// Update is one Telegram update. Exactly one of Message / CallbackQuery is typically set;
// the loop dispatches whatever arrived and lets the Handler decide.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

// Handler is the dispatch seam. The long-poll loop calls Handle once per update, in order.
// Later slices plug in the command router and the approval registry behind this interface;
// keeping it injectable means the loop owns polling and nothing else owns it.
type Handler interface {
	Handle(ctx context.Context, u Update)
}

// Gateway holds the long-poll loop's configuration and seams. Construct it with NewGateway.
type Gateway struct {
	apiBase     string // https://api.telegram.org/bot<token>
	httpClient  *http.Client
	offsets     *OffsetStore
	backoff     *risk.Backoff
	handler     Handler
	clock       func() time.Time
	sleep       func(context.Context, time.Duration)
	logf        func(string, ...any)
	pollTimeout int // long-poll hold time in seconds (0 = no wait, used in tests)
}

// Option configures a Gateway. The functional-option style mirrors
// internal/channel/telegram_live.go so the two Telegram surfaces read alike.
type Option func(*Gateway)

// WithHTTPClient injects the HTTP client (a test transport, or a client with a tuned
// timeout). A nil client is ignored.
func WithHTTPClient(c *http.Client) Option {
	return func(g *Gateway) {
		if c != nil {
			g.httpClient = c
		}
	}
}

// WithClock injects the clock. A nil clock is ignored.
func WithClock(now func() time.Time) Option {
	return func(g *Gateway) {
		if now != nil {
			g.clock = now
		}
	}
}

// WithSleep injects the sleep function (tests record the delay and return at once). A nil
// func is ignored.
func WithSleep(sleep func(context.Context, time.Duration)) Option {
	return func(g *Gateway) {
		if sleep != nil {
			g.sleep = sleep
		}
	}
}

// WithLogger injects the log sink. A nil func is ignored.
func WithLogger(logf func(string, ...any)) Option {
	return func(g *Gateway) {
		if logf != nil {
			g.logf = logf
		}
	}
}

// WithBackoff injects the back-off (reuses risk.Backoff — never reinvent the jitter). A
// nil back-off is ignored.
func WithBackoff(b *risk.Backoff) Option {
	return func(g *Gateway) {
		if b != nil {
			g.backoff = b
		}
	}
}

// WithPollTimeout sets the long-poll hold time in seconds. 0 means a non-blocking poll
// (used by tests so they never wait on the server). Negative values clamp to 0.
func WithPollTimeout(seconds int) Option {
	return func(g *Gateway) {
		if seconds < 0 {
			seconds = 0
		}
		g.pollTimeout = seconds
	}
}

// NewGateway builds a long-poll loop against apiBase (https://api.telegram.org/bot<token>,
// or an httptest server URL in tests), persisting progress to offsets and dispatching to
// handler. Sensible production defaults are filled for every seam; options override them.
func NewGateway(apiBase string, offsets *OffsetStore, handler Handler, opts ...Option) *Gateway {
	g := &Gateway{
		apiBase:     apiBase,
		httpClient:  &http.Client{},
		offsets:     offsets,
		backoff:     risk.NewBackoff(time.Second, 60*time.Second, time.Now().UnixNano()),
		handler:     handler,
		clock:       time.Now,
		sleep:       sleepCtx,
		logf:        func(string, ...any) {}, // silent by default; WithLogger wires a real sink
		pollTimeout: defaultPollTimeout,
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// sleepCtx is the production sleep: wait d, but wake early if ctx is canceled. This is the
// ONE seam whose default touches real time; tests inject a recorder instead.
func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Run is the always-on loop. It polls getUpdates from offsets.Load()+1 forever, dispatching
// each update to the handler in order and persisting progress after each batch. It returns
// ONLY when ctx is canceled (clean shutdown). Every other fault — transport error, 409, 429
// — is absorbed with a back-off and a retry; the loop never returns on a transient error.
func (g *Gateway) Run(ctx context.Context) error {
	// In-memory offset, seeded from the durable store so a restart resumes where we left
	// off. Persisted after each successful batch.
	offset := g.offsets.Load()
	attempt := 0

	for {
		// Fast cancel check so a canceled context never starts another poll.
		if err := ctx.Err(); err != nil {
			return g.shutdown(err)
		}

		updates, fault := g.poll(ctx, offset)
		if fault != nil {
			// A canceled context surfaces as a fault from poll; treat it as shutdown,
			// not as a transient error to back off on.
			if ctx.Err() != nil {
				return g.shutdown(ctx.Err())
			}
			g.handleFault(ctx, fault, attempt)
			attempt++
			continue
		}

		// Success: dispatch in order, advance the offset to the highest update id, persist,
		// and reset the back-off counter.
		if len(updates) > 0 {
			for _, u := range updates {
				g.safeHandle(ctx, u)
				if u.UpdateID > offset {
					offset = u.UpdateID
				}
			}
			if err := g.offsets.Save(offset); err != nil {
				// A failed persist is not fatal: we keep the in-memory offset so we won't
				// reprocess, and try to persist again next batch. Log it loudly.
				g.logf("gateway: failed to persist offset %d: %v", offset, err)
			}
		}
		attempt = 0
	}
}

// safeHandle dispatches one update to the handler, recovering from any panic so a
// single poison update (a malformed callback, a nil-deref in a command handler) can
// never kill the always-on loop. The offset still advances past the recovered update
// so it is not re-fetched. This is what makes the "never goes dark" guarantee hold
// against the handler seam, the surface most likely to evolve and misbehave.
func (g *Gateway) safeHandle(ctx context.Context, u Update) {
	defer func() {
		if r := recover(); r != nil {
			g.logf("gateway: handler panicked on update %d (recovered, continuing): %v", u.UpdateID, r)
		}
	}()
	g.handler.Handle(ctx, u)
}

// shutdown classifies the terminal context error. A plain cancellation is a clean stop and
// returns nil; any other context error (e.g. a deadline) is wrapped for the caller.
func (g *Gateway) shutdown(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		g.logf("gateway: shutting down (context canceled)")
		return nil
	}
	return fmt.Errorf("gateway: stopping: %w", err)
}

// fault carries why a poll did not yield updates: the kind (so handleFault can pick the
// right log + back-off) and an optional server-supplied retry-after.
type fault struct {
	kind       faultKind
	retryAfter time.Duration
	err        error
}

type faultKind int

const (
	faultTransport faultKind = iota // request failed, or a non-409/429 non-2xx status
	faultConflict                   // HTTP 409 — another process polled this token
	faultRateLimit                  // HTTP 429 — back off, honoring retry_after if given
)

func (f *fault) Error() string {
	if f.err != nil {
		return f.err.Error()
	}
	return "gateway fault"
}

// handleFault logs the fault appropriately and sleeps for the back-off delay. It NEVER
// returns control to the caller as an exit — by design the loop continues after this.
func (g *Gateway) handleFault(ctx context.Context, f *fault, attempt int) {
	switch f.kind {
	case faultConflict:
		// LOUD: a 409 means another process is polling this bot token. We do not crash
		// (the old tap-drop bug); we back off and keep ownership-contending gracefully.
		g.logf("gateway: WARNING another process is polling this bot token — getUpdates conflict; backing off")
	case faultRateLimit:
		g.logf("gateway: rate limited (429); backing off retry_after=%s", f.retryAfter)
	default:
		// SECRET HYGIENE: f.err is typically a *url.Error whose message embeds the
		// token-bearing apiBase. Redact the STRING we log; the error VALUE itself is
		// untouched so ctx-cancel detection / fault classification stay intact.
		g.logf("gateway: transport error polling getUpdates: %s; backing off", redactBase(f.err.Error(), g.apiBase))
	}
	g.sleep(ctx, g.backoff.Delay(attempt, f.retryAfter))
}

// poll performs one getUpdates request. On success it returns the decoded updates and a nil
// fault. On any failure it returns a typed fault for handleFault to act on. A canceled
// context is surfaced as a transport fault; Run distinguishes it from a real error.
func (g *Gateway) poll(ctx context.Context, offset int64) ([]Update, *fault) {
	// Per-request context: its deadline must EXCEED the long-poll hold so the HTTP client
	// never severs a legitimately-held poll early. It still honors the parent ctx, so a
	// shutdown cancels the in-flight request immediately.
	reqTimeout := time.Duration(g.pollTimeout)*time.Second + httpSlack
	reqCtx, cancel := context.WithTimeout(ctx, reqTimeout)
	defer cancel()

	q := url.Values{}
	q.Set("offset", strconv.FormatInt(offset+1, 10))
	q.Set("timeout", strconv.Itoa(g.pollTimeout))
	reqURL := g.apiBase + "/getUpdates?" + q.Encode()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, &fault{kind: faultTransport, err: fmt.Errorf("build getUpdates request: %w", err)}
	}

	res, err := g.httpClient.Do(req)
	if err != nil {
		return nil, &fault{kind: faultTransport, err: fmt.Errorf("getUpdates request failed: %w", err)}
	}
	defer func() { _ = res.Body.Close() }()

	// Read the body once; both the success decode and the 429 retry-after parse need it.
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, &fault{kind: faultTransport, err: fmt.Errorf("read getUpdates body: %w", err)}
	}

	switch {
	case res.StatusCode == http.StatusConflict: // 409
		return nil, &fault{kind: faultConflict, err: fmt.Errorf("getUpdates 409 conflict")}
	case res.StatusCode == http.StatusTooManyRequests: // 429
		return nil, &fault{
			kind:       faultRateLimit,
			retryAfter: parseRetryAfter(body),
			err:        fmt.Errorf("getUpdates 429 too many requests"),
		}
	case res.StatusCode < 200 || res.StatusCode >= 300:
		return nil, &fault{kind: faultTransport, err: fmt.Errorf("getUpdates status %d", res.StatusCode)}
	}

	var decoded struct {
		OK     bool     `json:"ok"`
		Result []Update `json:"result"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, &fault{kind: faultTransport, err: fmt.Errorf("decode getUpdates body: %w", err)}
	}
	if !decoded.OK {
		return nil, &fault{kind: faultTransport, err: fmt.Errorf("getUpdates returned ok=false")}
	}
	return decoded.Result, nil
}

// parseRetryAfter pulls parameters.retry_after (seconds) from a Telegram 429 body, returning
// 0 if absent or unparseable (the back-off then falls back to its jittered window).
func parseRetryAfter(body []byte) time.Duration {
	var p struct {
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return 0
	}
	if p.Parameters.RetryAfter <= 0 {
		return 0
	}
	return time.Duration(p.Parameters.RetryAfter) * time.Second
}
