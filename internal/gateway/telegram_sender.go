package gateway

// telegram_sender.go — TelegramSender is the OUTBOUND half of the gateway: it posts
// command replies and approval keyboards to Telegram's sendMessage. It is the concrete
// backing for two seams the rest of the package defines as interfaces:
//
//   - gateway.Sender          — Send(ctx, chatID, text) for plain command replies.
//   - gateway.KeyboardSender  — SendApprovalKeyboard(ctx, chatID, text, token) which
//     posts an inline Approve/Deny keyboard whose callback_data carries the routing
//     token in the EXACT "bucks:approve|deny:<token>" format the ApprovalRegistry
//     parses, so a tap routes back to the request waiting on it.
//
// It is the only place in the gateway that POSTS to Telegram; the always-on Run loop is
// the only place that POLLS. Keeping send and poll separate (but on the same bot token /
// apiBase) preserves the single-poller guarantee: nothing here calls getUpdates.
//
// Every external seam is injectable: the apiBase (an httptest URL in tests, the real
// https://api.telegram.org/bot<token> in production) and the *http.Client. No wall clock,
// no secret literal — the token is baked into apiBase by the caller from the loaded config.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// senderHTTPTimeout bounds a single sendMessage post. A reply or an approval keyboard is
// a short request; a generous-but-finite timeout keeps a wedged network from blocking the
// command/approval path forever while still tolerating a slow link.
const senderHTTPTimeout = 30 * time.Second

// TelegramSender posts command replies and approval keyboards to Telegram sendMessage.
// Construct it with NewTelegramSender against apiBase (https://api.telegram.org/bot<token>
// in production, or an httptest server URL in tests).
type TelegramSender struct {
	apiBase    string // https://api.telegram.org/bot<token> (or a test server URL)
	httpClient *http.Client
}

// SenderOption configures a TelegramSender.
type SenderOption func(*TelegramSender)

// WithSenderHTTPClient injects the HTTP client (a test transport, or a client with a
// tuned timeout). A nil client is ignored, leaving the default.
func WithSenderHTTPClient(c *http.Client) SenderOption {
	return func(s *TelegramSender) {
		if c != nil {
			s.httpClient = c
		}
	}
}

// NewTelegramSender builds a sender that posts to apiBase. apiBase already carries the
// bot token (the caller builds it as https://api.telegram.org/bot<token>); no secret is
// embedded here. The default HTTP client has a finite timeout; WithSenderHTTPClient
// overrides it (tests point at an httptest server).
func NewTelegramSender(apiBase string, opts ...SenderOption) *TelegramSender {
	s := &TelegramSender{
		apiBase:    apiBase,
		httpClient: &http.Client{Timeout: senderHTTPTimeout},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Send posts a plain text reply to chatID via sendMessage. It is the gateway.Sender
// implementation the CommandRouter uses for /status, /summary, /help, etc. No keyboard.
func (s *TelegramSender) Send(ctx context.Context, chatID int64, text string) error {
	return s.post(ctx, map[string]any{
		"chat_id": chatID,
		"text":    text,
	})
}

// SendApprovalKeyboard posts text to chatID with an inline keyboard whose Approve button
// carries callback_data "bucks:approve:<token>" and whose Deny button carries
// "bucks:deny:<token>" — the exact format ApprovalRegistry.parseCallbackData expects, so
// the operator's tap routes back to the waiting request. It is the gateway.KeyboardSender
// implementation the ApprovalRegistry posts through.
func (s *TelegramSender) SendApprovalKeyboard(ctx context.Context, chatID int64, text, token string) error {
	return s.post(ctx, map[string]any{
		"chat_id":      chatID,
		"text":         text,
		"reply_markup": approvalKeyboard(token),
	})
}

// approvalKeyboard builds the inline-keyboard reply_markup for an approval prompt: one row
// with an Approve and a Deny button, each carrying its namespaced callback_data token. The
// callback_data values are produced from the SAME namespace/action constants the registry
// parses, so the post side and the parse side can never drift apart.
func approvalKeyboard(token string) map[string]any {
	return map[string]any{
		"inline_keyboard": [][]map[string]any{
			{
				{"text": "✅ Approve", "callback_data": callbackData(actionApprove, token)},
				{"text": "🛑 Deny", "callback_data": callbackData(actionDeny, token)},
			},
		},
	}
}

// callbackData renders the "bucks:<action>:<token>" string the registry parses back. It is
// the inverse of parseCallbackData and shares its constants, so the encode/decode pair is
// guaranteed consistent.
func callbackData(action, token string) string {
	return callbackNamespace + ":" + action + ":" + token
}

// post marshals body to JSON and POSTs it to apiBase/sendMessage, honoring ctx. A non-2xx
// status is an error (so a failed approval keyboard post fails the request SAFE — Denied —
// rather than silently dropping the ask). It does not decode the response body.
func (s *TelegramSender) post(ctx context.Context, body map[string]any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("gateway: marshal sendMessage: %w", err)
	}
	url := s.apiBase + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		// SECRET HYGIENE: a URL-parse failure here embeds the token-bearing apiBase in its
		// message. Scrub before wrapping so a corrupted/odd token can never leak through
		// the returned error either (only a Do failure would in practice, but this closes
		// the whole construction path — no assumption that the base always parses).
		return fmt.Errorf("gateway: build sendMessage request: %w", errors.New(redactBase(err.Error(), s.apiBase)))
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := s.httpClient.Do(req)
	if err != nil {
		// SECRET HYGIENE: a Do() failure is a *url.Error whose message embeds the
		// token-bearing apiBase. Scrub the token BEFORE wrapping so every downstream
		// consumer (the command-reply log, the returned approval error) is token-free.
		// We re-wrap a redacted error rather than %w the raw one, so the secret is gone
		// from the whole error chain's string.
		return fmt.Errorf("gateway: sendMessage: %w", errors.New(redactBase(err.Error(), s.apiBase)))
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("gateway: sendMessage: telegram status %d", res.StatusCode)
	}
	return nil
}

// compile-time assertions: TelegramSender satisfies BOTH outbound seams.
var (
	_ Sender         = (*TelegramSender)(nil)
	_ KeyboardSender = (*TelegramSender)(nil)
)
