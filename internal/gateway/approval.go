package gateway

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"bucks/internal/channel"
)

// approval.go — the ApprovalRegistry is the ONE place an above-band approval is
// resolved, and it does so WITHOUT a second poller. It is a Handler that consumes the
// inline-button taps the always-on gateway already dispatches (callback_query updates
// only — text belongs to the CommandRouter), matches each tap to the pending request
// that is waiting on it, and hands back the operator's decision.
//
// Why this exists (the 409 tap-drop lesson): there must be exactly ONE owner of
// Telegram getUpdates in the whole program — the gateway long-poll loop. The old
// TelegramChannel.RequestApproval ran its OWN getUpdates poll while awaiting the tap,
// which would collide with the always-on gateway on the same bot token (HTTP 409,
// dropped taps). This registry removes that second poller: it posts the keyboard, then
// BLOCKS on an in-process channel that the gateway's dispatch fills when the matching
// tap arrives. One poller, fanned out by the Mux.
//
// FAIL-SAFE (unchanged): a timeout/cancel returns DecisionDenied + the ctx error; a
// post failure returns DecisionDenied + the wrapped error. A trade above the band is
// placed ONLY on an explicit Approve tap — silence is always a no.

// approvePrefix / denyPrefix are the callback_data namespaces the inline keyboard
// carries. The full callback_data is "bucks:<approve|deny>:<token>", where <token>
// uniquely identifies the pending request so concurrent approvals never cross.
const (
	callbackNamespace = "bucks"
	actionApprove     = "approve"
	actionDeny        = "deny"
)

// KeyboardSender posts an inline Approve/Deny keyboard whose buttons carry the routing
// token. It is the ONLY new transport capability this slice needs; SendAlert/SendReport
// are unchanged and are not part of this seam. The live wiring (a later slice in
// cmd/bucks) backs this with Telegram sendMessage + reply_markup; tests use a recorder.
type KeyboardSender interface {
	// SendApprovalKeyboard posts text to chatID with an inline keyboard whose Approve
	// button carries callback_data "bucks:approve:<token>" and whose Deny button
	// carries "bucks:deny:<token>".
	SendApprovalKeyboard(ctx context.Context, chatID int64, text, token string) error
}

// ApprovalRegistry routes operator taps to the requests waiting on them. It implements
// gateway.Handler (consuming callback_query updates) and channel.Approver (so the
// TelegramChannel can delegate its RequestApproval here — no self-poll).
type ApprovalRegistry struct {
	sender    KeyboardSender
	chatID    int64
	tokenFunc func() string

	mu      sync.Mutex
	pending map[string]chan channel.Decision

	counter atomic.Int64 // backs the default monotonic token generator
}

// ApprovalOption configures an ApprovalRegistry.
type ApprovalOption func(*ApprovalRegistry)

// WithTokenFunc injects the per-request token generator (tests inject a deterministic
// sequence). A nil func is ignored, leaving the default monotonic counter.
func WithTokenFunc(f func() string) ApprovalOption {
	return func(r *ApprovalRegistry) {
		if f != nil {
			r.tokenFunc = f
		}
	}
}

// WithChatID sets the operator chat id the registry posts approval keyboards to. The
// live wiring passes the trader's configured operator chat; tests that exercise the
// channel.Approver path set it, while the lower-level Request tests inject their own
// post func and do not need it.
func WithChatID(chatID int64) ApprovalOption {
	return func(r *ApprovalRegistry) { r.chatID = chatID }
}

// NewApprovalRegistry builds a registry that posts keyboards via sender. The default
// token generator is a monotonic counter; WithTokenFunc overrides it for tests.
func NewApprovalRegistry(sender KeyboardSender, opts ...ApprovalOption) *ApprovalRegistry {
	r := &ApprovalRegistry{
		sender:  sender,
		pending: make(map[string]chan channel.Decision),
	}
	r.tokenFunc = r.nextToken // default: monotonic counter
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// nextToken is the default monotonic token generator.
func (r *ApprovalRegistry) nextToken() string {
	return "a" + strconv.FormatInt(r.counter.Add(1), 10)
}

// Request registers a fresh token, invokes post (the caller actually posts the inline
// keyboard carrying that token), then blocks until the matching tap resolves it or ctx
// ends. It is the core fail-safe primitive:
//
//   - post error  -> DecisionDenied + wrapped error (couldn't even ask: no trade).
//   - ctx done    -> DecisionDenied + ctx error      (timeout/cancel: silence is a no).
//   - matched tap -> the operator's decision (Approve/Deny).
//
// The result channel is buffered (capacity 1) so the gateway dispatch goroutine that
// resolves it NEVER blocks, even if this waiter has already vanished (timed out).
func (r *ApprovalRegistry) Request(ctx context.Context, post func(ctx context.Context, token string) error) (channel.Decision, error) {
	token := r.tokenFunc()
	resultCh := make(chan channel.Decision, 1)

	r.mu.Lock()
	r.pending[token] = resultCh
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, token)
		r.mu.Unlock()
	}()

	if err := post(ctx, token); err != nil {
		// Could not even ask the operator -> fail-safe: no trade.
		return channel.DecisionDenied, fmt.Errorf("gateway: post approval request: %w", err)
	}

	select {
	case d := <-resultCh:
		return d, nil
	case <-ctx.Done():
		// Timeout or cancellation -> fail-safe DENIED. Silence is a no.
		return channel.DecisionDenied, ctx.Err()
	}
}

// RequestApproval makes the registry a channel.Approver: the TelegramChannel delegates
// here so it no longer self-polls. It posts the inline Approve/Deny keyboard (carrying
// the token) via the KeyboardSender and waits on the routed tap, preserving the
// fail-safe end to end.
func (r *ApprovalRegistry) RequestApproval(ctx context.Context, req channel.ApprovalRequest) (channel.Decision, error) {
	return r.Request(ctx, func(ctx context.Context, token string) error {
		return r.sender.SendApprovalKeyboard(ctx, r.chatID, req.Summary, token)
	})
}

// Handle consumes one update. It acts ONLY on callback_query taps that match our
// callback_data format and a currently-pending token; everything else (text messages,
// foreign callback_data, stale/unknown tokens, empty updates) is ignored without
// panic. It NEVER blocks the dispatch goroutine: the resolve is a non-blocking send to
// a buffered channel, so a vanished waiter cannot deadlock the always-on loop.
func (r *ApprovalRegistry) Handle(_ context.Context, u Update) {
	if u.CallbackQuery == nil {
		return // text belongs to the CommandRouter; not our update
	}
	action, token, ok := parseCallbackData(u.CallbackQuery.Data)
	if !ok {
		return // not our callback_data format
	}

	var decision channel.Decision
	switch action {
	case actionApprove:
		decision = channel.DecisionApproved
	case actionDeny:
		decision = channel.DecisionDenied
	default:
		return
	}

	r.mu.Lock()
	resultCh, found := r.pending[token]
	r.mu.Unlock()
	if !found {
		return // stale/unknown token (already resolved, timed out, or never ours)
	}

	// Non-blocking send: the channel is buffered (cap 1) and unregistered exactly once
	// by the waiter, so this never blocks the dispatch goroutine. If a duplicate tap
	// arrives after the buffer is filled, the default arm drops it harmlessly.
	select {
	case resultCh <- decision:
	default:
	}
}

// parseCallbackData splits "bucks:<approve|deny>:<token>" into its action and token.
// It returns ok=false for any string that is not in our namespace/format or that has
// an empty token, so a foreign or malformed callback_data is ignored safely.
func parseCallbackData(data string) (action, token string, ok bool) {
	parts := strings.SplitN(data, ":", 3)
	if len(parts) != 3 {
		return "", "", false
	}
	if parts[0] != callbackNamespace {
		return "", "", false
	}
	if parts[2] == "" {
		return "", "", false // empty token never matches a real pending request
	}
	return parts[1], parts[2], true
}

// compile-time assertions: the registry is both a gateway Handler and a channel.Approver.
var (
	_ Handler          = (*ApprovalRegistry)(nil)
	_ channel.Approver = (*ApprovalRegistry)(nil)
)
