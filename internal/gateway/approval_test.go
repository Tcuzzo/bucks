package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"bucks/internal/channel"
)

// recordingKeyboardSender records every Approve/Deny keyboard post (the token it
// carried) so a test can assert the registry posted before blocking. It can also be
// scripted to fail (post error) to exercise the fail-safe. No network.
type recordingKeyboardSender struct {
	mu     sync.Mutex
	tokens []string
	chats  []int64
	texts  []string
	err    error
}

func (s *recordingKeyboardSender) SendApprovalKeyboard(_ context.Context, chatID int64, text, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.tokens = append(s.tokens, token)
	s.chats = append(s.chats, chatID)
	s.texts = append(s.texts, text)
	return nil
}

func (s *recordingKeyboardSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tokens)
}

// callbackUpdate builds a callback_query Update carrying callback_data.
func callbackUpdate(updateID int64, data string) Update {
	return Update{UpdateID: updateID, CallbackQuery: &CallbackQuery{ID: "cb", Data: data}}
}

// 1a. A routed Approve callback for the registered token resolves the Request as Approved.
func TestRegistryApproveResolvesApproved(t *testing.T) {
	ks := &recordingKeyboardSender{}
	tokens := make(chan string, 1)
	reg := NewApprovalRegistry(ks, WithTokenFunc(seqTokens()))

	go func() {
		// Wait until the Request has posted (so the token exists), then route an Approve.
		tok := <-tokens
		reg.Handle(context.Background(), callbackUpdate(1, "bucks:approve:"+tok))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, err := reg.Request(ctx, func(_ context.Context, token string) error {
		tokens <- token
		return nil
	})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}
	if !d.Approved() {
		t.Fatalf("routed Approve must resolve Approved, got %s", d)
	}
	if ks.count() != 0 {
		t.Fatalf("Request used the injected post func, the keyboard sender should not be called directly here")
	}
}

// 1b. A routed Deny callback resolves the Request as Denied.
func TestRegistryDenyResolvesDenied(t *testing.T) {
	reg := NewApprovalRegistry(&recordingKeyboardSender{}, WithTokenFunc(seqTokens()))
	tokens := make(chan string, 1)

	go func() {
		tok := <-tokens
		reg.Handle(context.Background(), callbackUpdate(1, "bucks:deny:"+tok))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, err := reg.Request(ctx, func(_ context.Context, token string) error {
		tokens <- token
		return nil
	})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}
	if d.Approved() {
		t.Fatalf("routed Deny must resolve Denied, got %s", d)
	}
}

// 2. Fail-safe timeout: a Request whose ctx deadline elapses with no callback returns
// DecisionDenied + ctx error; nothing is approved.
func TestRegistryTimeoutDenies(t *testing.T) {
	reg := NewApprovalRegistry(&recordingKeyboardSender{}, WithTokenFunc(seqTokens()))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	d, err := reg.Request(ctx, func(_ context.Context, _ string) error { return nil })
	if d.Approved() {
		t.Fatalf("a deadline with no tap must be Denied, got %s", d)
	}
	if err == nil {
		t.Fatalf("expected the ctx deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

// 3. Fail-safe post error: when post returns an error, Request returns DecisionDenied,
// wraps the error, and does not hang.
func TestRegistryPostErrorDenies(t *testing.T) {
	reg := NewApprovalRegistry(&recordingKeyboardSender{}, WithTokenFunc(seqTokens()))
	postErr := errors.New("could not post keyboard")

	done := make(chan struct{})
	var d channel.Decision
	var err error
	go func() {
		d, err = reg.Request(context.Background(), func(_ context.Context, _ string) error {
			return postErr
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Request hung on a post error — must fail fast as Denied")
	}
	if d.Approved() {
		t.Fatalf("a post error must be Denied, got %s", d)
	}
	if !errors.Is(err, postErr) {
		t.Fatalf("expected the post error to be wrapped (%%w), got %v", err)
	}
}

// 4. Token isolation: two concurrent Requests (T1, T2); a callback for T2 resolves ONLY
// the T2 waiter; T1 still waits and then times out. No cross-talk.
func TestRegistryTokenIsolation(t *testing.T) {
	reg := NewApprovalRegistry(&recordingKeyboardSender{}, WithTokenFunc(seqTokens()))

	tok1 := make(chan string, 1)
	tok2 := make(chan string, 1)

	// T1: short deadline, no callback ever routed to it -> must time out Denied.
	r1done := make(chan channel.Decision, 1)
	r1err := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		d, err := reg.Request(ctx, func(_ context.Context, token string) error {
			tok1 <- token
			return nil
		})
		r1done <- d
		r1err <- err
	}()

	// T2: a generous deadline; we route an Approve for ITS token only.
	r2done := make(chan channel.Decision, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		d, _ := reg.Request(ctx, func(_ context.Context, token string) error {
			tok2 <- token
			return nil
		})
		r2done <- d
	}()

	t1 := <-tok1
	t2 := <-tok2
	if t1 == t2 {
		t.Fatalf("two concurrent Requests must get distinct tokens, both got %q", t1)
	}

	// Route an Approve for T2 only.
	reg.Handle(context.Background(), callbackUpdate(2, "bucks:approve:"+t2))

	if d := <-r2done; !d.Approved() {
		t.Fatalf("T2 must resolve Approved from its own callback, got %s", d)
	}
	// T1 received no callback -> it must still time out Denied (no cross-talk).
	if d := <-r1done; d.Approved() {
		t.Fatalf("T1 must NOT be approved by a T2 callback (cross-talk), got %s", d)
	}
	if err := <-r1err; err == nil {
		t.Fatalf("T1 must surface its deadline error")
	}
}

// 5. A stale/unknown token callback is ignored without panic and never blocks the
// dispatch goroutine (Handle must return promptly even with no waiter registered).
func TestRegistryStaleCallbackIgnored(t *testing.T) {
	reg := NewApprovalRegistry(&recordingKeyboardSender{}, WithTokenFunc(seqTokens()))

	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Handle panicked on a stale callback: %v", r)
			}
			close(done)
		}()
		// No Request registered any token; these must be ignored safely.
		reg.Handle(context.Background(), callbackUpdate(1, "bucks:approve:does-not-exist"))
		reg.Handle(context.Background(), callbackUpdate(2, "garbage-not-our-format"))
		reg.Handle(context.Background(), callbackUpdate(3, "bucks:approve:")) // empty token
		reg.Handle(context.Background(), Update{UpdateID: 4})                 // neither message nor callback
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Handle blocked on a stale callback — it must never block the dispatch goroutine")
	}
}

// Concurrency: many waiters + their resolving callbacks under -race, proving the map
// guard and the non-blocking resolve hold. A vanished waiter (timed out) whose callback
// arrives late must not panic or block.
func TestRegistryConcurrentResolve(t *testing.T) {
	reg := NewApprovalRegistry(&recordingKeyboardSender{}, WithTokenFunc(seqTokens()))

	const n = 40
	var wg sync.WaitGroup
	approved := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tokc := make(chan string, 1)
			// Resolve each request from another goroutine once its token is known.
			go func() {
				tok := <-tokc
				reg.Handle(context.Background(), callbackUpdate(int64(100+i), "bucks:approve:"+tok))
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			d, _ := reg.Request(ctx, func(_ context.Context, token string) error {
				tokc <- token
				return nil
			})
			approved[i] = d.Approved()
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if !approved[i] {
			t.Fatalf("request %d was not approved by its own callback under concurrency", i)
		}
	}

	// Late callback for a vanished waiter must not panic or block.
	reg.Handle(context.Background(), callbackUpdate(9999, "bucks:approve:stale"))
}

// seqTokens returns a deterministic monotonic token generator for tests.
func seqTokens() func() string {
	var mu sync.Mutex
	var n int64
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return fmt.Sprintf("t%d", n)
	}
}
