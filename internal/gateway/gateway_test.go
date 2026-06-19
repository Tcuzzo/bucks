package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"bucks/internal/risk"
)

// recordingHandler captures the updates dispatched to it, in order, under a mutex
// so the test can assert delivery without racing the Run goroutine.
type recordingHandler struct {
	mu      sync.Mutex
	updates []Update
	// onHandle, if set, runs after each update is recorded — used by tests to
	// cancel the context once the expected updates have arrived.
	onHandle func(u Update, total int)
}

func (h *recordingHandler) Handle(_ context.Context, u Update) {
	h.mu.Lock()
	h.updates = append(h.updates, u)
	n := len(h.updates)
	cb := h.onHandle
	h.mu.Unlock()
	if cb != nil {
		cb(u, n)
	}
}

func (h *recordingHandler) snapshot() []Update {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Update, len(h.updates))
	copy(out, h.updates)
	return out
}

// sleepRecorder records every delay the loop asked to sleep for and returns
// immediately (honoring ctx) so tests never actually wait.
type sleepRecorder struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) {
	s.mu.Lock()
	s.delays = append(s.delays, d)
	s.mu.Unlock()
	// Honor cancellation but never actually block on the clock.
	select {
	case <-ctx.Done():
	default:
	}
}

func (s *sleepRecorder) snapshot() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]time.Duration, len(s.delays))
	copy(out, s.delays)
	return out
}

func (s *sleepRecorder) maxDelay() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	var max time.Duration
	for _, d := range s.delays {
		if d > max {
			max = d
		}
	}
	return max
}

// okResponse builds a Telegram getUpdates 200 body carrying the given updates.
func okResponse(updates ...map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"ok": true, "result": updates})
	return b
}

func msgUpdate(updateID int64, chatID int64, text string) map[string]any {
	return map[string]any{
		"update_id": updateID,
		"message": map[string]any{
			"message_id": updateID, // arbitrary but stable
			"chat":       map[string]any{"id": chatID},
			"text":       text,
		},
	}
}

// newOffsetStore returns a store backed by a temp file unique to the test.
func newOffsetStore(t *testing.T) *OffsetStore {
	t.Helper()
	return NewOffsetStore(filepath.Join(t.TempDir(), "offset.json"))
}

// fastBackoff is a backoff with a tiny base/cap so any jitter the loop computes is
// trivially small; deterministic seed for reproducibility.
func fastBackoff() *risk.Backoff {
	return risk.NewBackoff(time.Millisecond, 100*time.Millisecond, 1)
}

// panicTextHandler panics when it sees an update whose message text equals panicOn,
// and records the text of every other update. Used to prove a poison update can't
// kill the always-on loop.
type panicTextHandler struct {
	mu       sync.Mutex
	handled  []string
	panicOn  string
	onHandle func(total int)
}

func (h *panicTextHandler) Handle(_ context.Context, u Update) {
	if u.Message != nil && u.Message.Text == h.panicOn {
		panic("poison update in handler")
	}
	h.mu.Lock()
	if u.Message != nil {
		h.handled = append(h.handled, u.Message.Text)
	}
	n := len(h.handled)
	cb := h.onHandle
	h.mu.Unlock()
	if cb != nil {
		cb(n)
	}
}

func (h *panicTextHandler) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.handled))
	copy(out, h.handled)
	return out
}

// TestRun_SurvivesHandlerPanic proves a panicking handler (a poison update) does NOT
// kill the always-on loop: the loop recovers, advances the offset PAST the poison so
// it is never re-fetched, and keeps dispatching subsequent updates. For a trading
// agent this is the guarantee that one bad update can't make the /halt path go dark.
func TestRun_SurvivesHandlerPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(okResponse(
			msgUpdate(50, 5, "BOOM"), // handler panics on this one
			msgUpdate(51, 5, "ok"),   // must still be delivered
		))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	offsets := newOffsetStore(t)
	h := &panicTextHandler{panicOn: "BOOM"}
	h.onHandle = func(total int) {
		if total >= 1 { // the good "ok" update arrived -> we survived the panic
			cancel()
		}
	}
	sr := &sleepRecorder{}
	g := NewGateway(srv.URL, offsets, h,
		WithSleep(sr.sleep), WithBackoff(fastBackoff()), WithPollTimeout(0))

	_ = g.Run(ctx)

	if got := h.snapshot(); len(got) == 0 || got[0] != "ok" {
		t.Fatalf("good update after the poison was not delivered; handled=%v", got)
	}
	if off := offsets.Load(); off != 51 {
		t.Errorf("offset = %d, want 51 (advanced past the poison update so it is never re-fetched)", off)
	}
}

// Test 1 — happy path: two updates delivered in order, offset persisted to the last id.
func TestRun_HappyPath_DeliversInOrderAndPersistsOffset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always return the two updates; the loop will keep polling, but once it
		// has dispatched both the handler cancels the context.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(okResponse(
			msgUpdate(101, 5, "first"),
			msgUpdate(102, 5, "second"),
		))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	offsets := newOffsetStore(t)
	h := &recordingHandler{}
	h.onHandle = func(_ Update, total int) {
		if total >= 2 {
			cancel()
		}
	}
	sr := &sleepRecorder{}

	g := NewGateway(srv.URL, offsets, h,
		WithSleep(sr.sleep),
		WithBackoff(fastBackoff()),
		WithPollTimeout(0), // no real long-poll wait in tests
	)

	err := g.Run(ctx)
	if err != nil && err != context.Canceled {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	got := h.snapshot()
	if len(got) < 2 {
		t.Fatalf("expected >=2 updates, got %d", len(got))
	}
	if got[0].UpdateID != 101 || got[1].UpdateID != 102 {
		t.Fatalf("updates out of order: %d then %d", got[0].UpdateID, got[1].UpdateID)
	}
	if got[0].Message == nil || got[0].Message.Text != "first" {
		t.Fatalf("first update message wrong: %+v", got[0].Message)
	}
	if got[0].Message.Chat.ID != 5 {
		t.Fatalf("chat id not parsed: %+v", got[0].Message)
	}
	if persisted := offsets.Load(); persisted != 102 {
		t.Fatalf("offset not persisted to last update id: got %d want 102", persisted)
	}
}

// Test 2 — reconnect after a transport-level failure: the first request 500s, the loop
// backs off (sleep seam invoked) and does NOT exit, then the next request delivers.
func TestRun_ReconnectsAfterTransportFailure(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(okResponse(msgUpdate(7, 3, "after recovery")))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	offsets := newOffsetStore(t)
	h := &recordingHandler{}
	h.onHandle = func(_ Update, _ int) { cancel() }
	sr := &sleepRecorder{}

	g := NewGateway(srv.URL, offsets, h,
		WithSleep(sr.sleep),
		WithBackoff(fastBackoff()),
		WithPollTimeout(0),
	)

	if err := g.Run(ctx); err != nil && err != context.Canceled {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	got := h.snapshot()
	if len(got) != 1 || got[0].UpdateID != 7 {
		t.Fatalf("expected the post-recovery update 7 delivered, got %+v", got)
	}
	if len(sr.snapshot()) == 0 {
		t.Fatalf("expected the loop to back off (sleep) after the transport failure; no sleep recorded")
	}
}

// Test 3 — 409 Conflict: two 409s then a 200. The loop must not crash/exit, must back off,
// and must eventually deliver.
func TestRun_HandlesConflict409_WithoutExiting(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":409,"description":"Conflict: terminated by other getUpdates request"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(okResponse(msgUpdate(42, 9, "won the conflict")))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	offsets := newOffsetStore(t)
	h := &recordingHandler{}
	h.onHandle = func(_ Update, _ int) { cancel() }
	sr := &sleepRecorder{}

	g := NewGateway(srv.URL, offsets, h,
		WithSleep(sr.sleep),
		WithBackoff(fastBackoff()),
		WithPollTimeout(0),
	)

	if err := g.Run(ctx); err != nil && err != context.Canceled {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	got := h.snapshot()
	if len(got) != 1 || got[0].UpdateID != 42 {
		t.Fatalf("expected update 42 delivered after the 409 storm, got %+v", got)
	}
	if len(sr.snapshot()) < 2 {
		t.Fatalf("expected the loop to back off on each 409 (>=2 sleeps), got %d", len(sr.snapshot()))
	}
}

// Test 4 — 429 retry_after: the server says wait 7s; the sleep seam must be asked for a
// delay reflecting that (>= 7s). Then a 200 delivers.
func TestRun_Handles429_RetryAfter(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":7}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(okResponse(msgUpdate(8, 2, "after rate limit")))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	offsets := newOffsetStore(t)
	h := &recordingHandler{}
	h.onHandle = func(_ Update, _ int) { cancel() }
	sr := &sleepRecorder{}

	// Cap must allow a 7s delay through (retryAfter is clamped to cap by Backoff).
	bo := risk.NewBackoff(time.Millisecond, time.Minute, 1)

	g := NewGateway(srv.URL, offsets, h,
		WithSleep(sr.sleep),
		WithBackoff(bo),
		WithPollTimeout(0),
	)

	if err := g.Run(ctx); err != nil && err != context.Canceled {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	got := h.snapshot()
	if len(got) != 1 || got[0].UpdateID != 8 {
		t.Fatalf("expected update 8 delivered after the 429, got %+v", got)
	}
	if max := sr.maxDelay(); max < 7*time.Second {
		t.Fatalf("expected a sleep >= 7s reflecting retry_after, got max %v", max)
	}
}

// Test 5 — clean shutdown: cancel before the first poll completes → Run returns promptly
// and does not keep polling.
func TestRun_CleanShutdownOnCancel(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(okResponse()) // empty result set, no updates
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately: the loop should observe ctx.Done() and return without
	// dispatching anything, promptly.
	cancel()

	offsets := newOffsetStore(t)
	h := &recordingHandler{}
	sr := &sleepRecorder{}

	g := NewGateway(srv.URL, offsets, h,
		WithSleep(sr.sleep),
		WithBackoff(fastBackoff()),
		WithPollTimeout(0),
	)

	done := make(chan error, 1)
	go func() { done <- g.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Fatalf("clean shutdown should return nil or context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after ctx cancel")
	}

	if len(h.snapshot()) != 0 {
		t.Fatalf("no updates should be dispatched after immediate cancel, got %d", len(h.snapshot()))
	}
}

// compile-time assertion that recordingHandler satisfies the dispatch seam.
var _ Handler = (*recordingHandler)(nil)
