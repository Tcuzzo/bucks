//go:build telegram_live

// Package channel — TelegramChannel is the LIVE operator surface over the Telegram
// Bot API. It is compiled ONLY under the `telegram_live` build tag, so it is never
// part of the default test suite and a normal `go build ./...` / `go test ./...`
// can NEVER make a live network call from this package. Live integration is opt-in
// and env-keyed.
//
// Spec §4.8 / §6: BUCKS ships with his OWN Telegram bot token slot and NEVER shares
// a token (the 409 tap-drop lesson — one agent, one bot). The token and chat id are
// read from the environment at construction (BUCKS_TELEGRAM_BOT_TOKEN /
// BUCKS_TELEGRAM_CHAT_ID); NO secret is embedded in source. NewTelegramChannel
// fails loudly if the token is absent rather than running tokenless.
//
// FAIL-SAFE APPROVAL: RequestApproval polls getUpdates for the operator's tap on an
// inline "Approve / Deny" keyboard until the context deadline; a timeout or any
// transport error returns DecisionDenied (never Approved). Silence is a no.
package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Env var names for the trader's OWN bot credentials. No secret literal is ever
// committed; these are read at runtime only.
const (
	envTelegramToken  = "BUCKS_TELEGRAM_BOT_TOKEN"
	envTelegramChatID = "BUCKS_TELEGRAM_CHAT_ID"
)

// callbackApprove / callbackDeny are the inline-button callback_data values the
// operator's tap returns. They are matched verbatim when polling updates.
const (
	callbackApprove = "bucks_approve"
	callbackDeny    = "bucks_deny"
)

// TelegramChannel is the live Bot API implementation of Channel. It owns its own
// HTTP client and bot token; it shares no token with any other agent.
type TelegramChannel struct {
	httpClient *http.Client
	apiBase    string // https://api.telegram.org/bot<token>
	chatID     string
	pollEvery  time.Duration // how often to poll getUpdates while awaiting approval
	now        func() time.Time
}

// TelegramOption configures a TelegramChannel.
type TelegramOption func(*TelegramChannel)

// WithHTTPClient injects the HTTP client (e.g. a tighter-timeout client or a test
// transport). A nil client is ignored.
func WithHTTPClient(c *http.Client) TelegramOption {
	return func(t *TelegramChannel) {
		if c != nil {
			t.httpClient = c
		}
	}
}

// WithPollInterval sets how often RequestApproval polls for the operator's tap.
func WithPollInterval(d time.Duration) TelegramOption {
	return func(t *TelegramChannel) {
		if d > 0 {
			t.pollEvery = d
		}
	}
}

// WithClock injects the clock (tests pass a controllable clock).
func WithClock(now func() time.Time) TelegramOption {
	return func(t *TelegramChannel) {
		if now != nil {
			t.now = now
		}
	}
}

// NewTelegramChannel builds a live channel from the trader's OWN bot token and
// chat id, read from the environment (never embedded). It returns an error if the
// token is missing — BUCKS never runs the live channel tokenless.
func NewTelegramChannel(opts ...TelegramOption) (*TelegramChannel, error) {
	token := strings.TrimSpace(os.Getenv(envTelegramToken))
	if token == "" {
		return nil, fmt.Errorf("channel: %s is not set — BUCKS needs his OWN bot token (never shared)", envTelegramToken)
	}
	chatID := strings.TrimSpace(os.Getenv(envTelegramChatID))
	if chatID == "" {
		return nil, fmt.Errorf("channel: %s is not set — no operator chat to reach", envTelegramChatID)
	}
	t := &TelegramChannel{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		apiBase:    "https://api.telegram.org/bot" + token,
		chatID:     chatID,
		pollEvery:  2 * time.Second,
		now:        time.Now,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t, nil
}

// SendAlert posts a one-way message via sendMessage.
func (t *TelegramChannel) SendAlert(ctx context.Context, a Alert) error {
	body := map[string]any{
		"chat_id": t.chatID,
		"text":    fmt.Sprintf("[%s] %s", a.Level, a.Text),
	}
	return t.post(ctx, "sendMessage", body, nil)
}

// SendReport posts a formatted report via sendMessage.
func (t *TelegramChannel) SendReport(ctx context.Context, r Report) error {
	body := map[string]any{
		"chat_id": t.chatID,
		"text":    formatReport(r),
	}
	return t.post(ctx, "sendMessage", body, nil)
}

// RequestApproval posts an inline Approve/Deny keyboard and polls getUpdates until
// the operator taps a button or the context deadline elapses. A timeout or any
// transport error returns the fail-safe DecisionDenied — silence is a no.
func (t *TelegramChannel) RequestApproval(ctx context.Context, r ApprovalRequest) (Decision, error) {
	keyboard := map[string]any{
		"inline_keyboard": [][]map[string]string{{
			{"text": "✅ Approve", "callback_data": callbackApprove},
			{"text": "🛑 Deny", "callback_data": callbackDeny},
		}},
	}
	body := map[string]any{
		"chat_id":      t.chatID,
		"text":         r.Summary,
		"reply_markup": keyboard,
	}
	if err := t.post(ctx, "sendMessage", body, nil); err != nil {
		return DecisionDenied, err // fail-safe: could not even ask -> no trade
	}

	var lastUpdateID int64
	ticker := time.NewTicker(t.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return DecisionDenied, ctx.Err() // timeout/cancel -> fail-safe DENIED
		case <-ticker.C:
			decision, matched, newOffset, err := t.pollApproval(ctx, lastUpdateID)
			if err != nil {
				// Transport hiccup while polling: keep waiting until the deadline
				// rather than treating it as approval. Only the deadline (above)
				// ends the wait, and it ends it as Denied.
				continue
			}
			lastUpdateID = newOffset
			if matched {
				return decision, nil
			}
		}
	}
}

// pollApproval fetches updates after offset and looks for the operator's callback
// tap. It returns the decision and matched=true when a button tap is seen, plus
// the new offset to poll from next.
func (t *TelegramChannel) pollApproval(ctx context.Context, offset int64) (Decision, bool, int64, error) {
	body := map[string]any{"offset": offset + 1, "timeout": 0}
	var resp struct {
		OK     bool `json:"ok"`
		Result []struct {
			UpdateID      int64 `json:"update_id"`
			CallbackQuery *struct {
				Data string `json:"data"`
			} `json:"callback_query"`
		} `json:"result"`
	}
	if err := t.post(ctx, "getUpdates", body, &resp); err != nil {
		return DecisionDenied, false, offset, err
	}
	newOffset := offset
	for _, u := range resp.Result {
		if u.UpdateID > newOffset {
			newOffset = u.UpdateID
		}
		if u.CallbackQuery == nil {
			continue
		}
		switch u.CallbackQuery.Data {
		case callbackApprove:
			return DecisionApproved, true, newOffset, nil
		case callbackDeny:
			return DecisionDenied, true, newOffset, nil
		}
	}
	return DecisionDenied, false, newOffset, nil
}

// post sends a JSON POST to method and, when out != nil, decodes the response into
// it. A non-2xx status is an error. The request honors the context.
func (t *TelegramChannel) post(ctx context.Context, method string, body map[string]any, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("channel: marshal %s: %w", method, err)
	}
	url := t.apiBase + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("channel: build %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("channel: %s: %w", method, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("channel: %s: telegram status %d", method, res.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			return fmt.Errorf("channel: decode %s: %w", method, err)
		}
	}
	return nil
}

// formatReport renders a Report as plain text for Telegram.
func formatReport(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "BUCKS report @ %s\n", r.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "Equity: %s | Realized PnL: %s | Unrealized PnL: %s\n",
		r.Equity.String(), r.RealizedPL.String(), r.UnrealizedPL.String())
	if len(r.Positions) > 0 {
		b.WriteString("Positions:\n")
		for _, p := range r.Positions {
			fmt.Fprintf(&b, "  %s qty=%s mark=%s uPnL=%s\n",
				p.Symbol, p.Qty.String(), p.MarkPx.String(), p.UnrealizedPL.String())
		}
	}
	if len(r.Rationales) > 0 {
		b.WriteString("Recent:\n")
		for _, t := range r.Rationales {
			fmt.Fprintf(&b, "  %s %s qty=%s — %s\n", t.Side, t.Symbol, t.Qty.String(), t.Reason)
		}
	}
	return b.String()
}

// compile-time assertion that TelegramChannel satisfies the interface (under tag).
var _ Channel = (*TelegramChannel)(nil)
