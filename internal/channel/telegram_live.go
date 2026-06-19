// Package channel — TelegramChannel is the LIVE operator surface over the Telegram
// Bot API. It is compiled into the default build (it ships in the binary), yet a
// normal `go test ./...` still makes NO real network call: the tests drive a local
// httptest server, and the dial-blocker in nolive_test.go (TestMain) counts and
// refuses every real outbound dial. So the "no live call in the default suite"
// guarantee is behavioral, not a build fence.
//
// Spec §4.8 / §6: BUCKS ships with his OWN Telegram bot token slot and NEVER shares
// a token (the 409 tap-drop lesson — one agent, one bot). The token and chat id come
// from an explicit WithToken/WithChatID (the daemon feeds the wizard-saved secret) or
// fall back to the environment (BUCKS_TELEGRAM_BOT_TOKEN / BUCKS_TELEGRAM_CHAT_ID); NO
// secret is embedded in source. NewTelegramChannel fails loudly if NEITHER yields a
// token rather than running tokenless.
//
// SINGLE POLLER: this channel does NOT poll getUpdates. SendAlert and SendReport are
// one-way posts (sendMessage); an above-band approval is delegated to an injected
// Approver — in production the gateway's ApprovalRegistry, which resolves the
// operator's tap from the SAME update stream the always-on gateway already polls.
// There is exactly one owner of getUpdates in the program (the gateway), so this
// channel can never collide with it on the bot token (no second poller).
//
// FAIL-SAFE APPROVAL: RequestApproval returns whatever the Approver decides; the
// Approver's contract is that a timeout or any transport/post error yields
// DecisionDenied (never Approved). Silence is a no. If no Approver is wired,
// RequestApproval fails SAFE with DecisionDenied rather than placing an above-band
// trade unapproved.
package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Env var names for the trader's OWN bot credentials. No secret literal is ever
// committed; these are read at runtime only.
const (
	envTelegramToken  = "BUCKS_TELEGRAM_BOT_TOKEN"
	envTelegramChatID = "BUCKS_TELEGRAM_CHAT_ID"
)

// TelegramChannel is the live Bot API implementation of Channel. It owns its own
// HTTP client and bot token; it shares no token with any other agent. It does NOT
// poll getUpdates — approvals are delegated to an injected Approver so the gateway
// remains the single poller.
type TelegramChannel struct {
	httpClient *http.Client
	apiBase    string // https://api.telegram.org/bot<token>
	chatID     string
	approver   Approver // resolves above-band approvals (the gateway registry); no self-poll
}

// telegramConfig collects the construction inputs an Option can set explicitly. An
// explicit value (from the wizard-saved secret the daemon feeds in) takes precedence over
// the environment fallback, so a wizard-only operator who never exported the env var still
// gets a working channel — and the channel can never diverge from the gateway's token.
type telegramConfig struct {
	token  string // explicit bot token; "" -> fall back to the env var
	chatID string // explicit operator chat id; "" -> fall back to the env var
}

// TelegramOption configures a TelegramChannel. A few options act on the live struct after
// construction (the HTTP client, the approver); WithToken/WithChatID act on the construction
// inputs (resolved in NewTelegramChannel before the struct is built).
type TelegramOption func(*telegramConfig, *TelegramChannel)

// WithToken sets the bot token explicitly (the daemon passes the wizard-saved secret here),
// taking precedence over BUCKS_TELEGRAM_BOT_TOKEN. An empty token is ignored (env fallback
// applies), so this never weakens the "needs a token" guarantee.
func WithToken(tok string) TelegramOption {
	return func(c *telegramConfig, _ *TelegramChannel) {
		if strings.TrimSpace(tok) != "" {
			c.token = strings.TrimSpace(tok)
		}
	}
}

// WithChatID sets the operator chat id explicitly (the daemon passes the trusted chat id
// from the SAME source the gateway uses), taking precedence over BUCKS_TELEGRAM_CHAT_ID. A
// zero id is ignored (env fallback applies).
func WithChatID(id int64) TelegramOption {
	return func(c *telegramConfig, _ *TelegramChannel) {
		if id != 0 {
			c.chatID = strconv.FormatInt(id, 10)
		}
	}
}

// WithHTTPClient injects the HTTP client (e.g. a tighter-timeout client or a test
// transport). A nil client is ignored.
func WithHTTPClient(c *http.Client) TelegramOption {
	return func(_ *telegramConfig, t *TelegramChannel) {
		if c != nil {
			t.httpClient = c
		}
	}
}

// WithApprover injects the approval backend at construction. In production this is the
// gateway's ApprovalRegistry; tests inject a fake. A nil approver is ignored (it can be
// wired later via SetApprover once the gateway is constructed).
func WithApprover(a Approver) TelegramOption {
	return func(_ *telegramConfig, t *TelegramChannel) {
		if a != nil {
			t.approver = a
		}
	}
}

// NewTelegramChannel builds a live channel from the trader's OWN bot token and chat id.
// The token/chat id come from an EXPLICIT WithToken/WithChatID (the daemon feeds the
// wizard-saved secret — so a wizard-only operator who never exported the env var still
// gets a channel, and it can never diverge from the gateway's token) and otherwise FALL
// BACK to the environment (BUCKS_TELEGRAM_BOT_TOKEN / BUCKS_TELEGRAM_CHAT_ID), preserving
// the existing env-only callers. It returns an error if NEITHER yields a token — BUCKS
// never runs the live channel tokenless.
func NewTelegramChannel(opts ...TelegramOption) (*TelegramChannel, error) {
	// Apply the construction-input options first (WithToken/WithChatID). They act on cfg;
	// the struct-mutating options (WithHTTPClient/WithApprover) act on t and run after the
	// struct exists. Splitting the two phases keeps explicit-over-env precedence clean.
	cfg := telegramConfig{}
	t := &TelegramChannel{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(&cfg, t)
	}

	// Explicit value (from cfg) wins; otherwise fall back to the env var.
	token := cfg.token
	if token == "" {
		token = strings.TrimSpace(os.Getenv(envTelegramToken))
	}
	if token == "" {
		return nil, fmt.Errorf("channel: no bot token (pass WithToken or set %s) — BUCKS needs his OWN bot token (never shared)", envTelegramToken)
	}
	chatID := cfg.chatID
	if chatID == "" {
		chatID = strings.TrimSpace(os.Getenv(envTelegramChatID))
	}
	if chatID == "" {
		return nil, fmt.Errorf("channel: no operator chat id (pass WithChatID or set %s) — no operator chat to reach", envTelegramChatID)
	}

	t.apiBase = "https://api.telegram.org/bot" + token
	t.chatID = chatID
	return t, nil
}

// SetApprover wires (or rewires) the approval backend after construction. The live
// wiring in cmd/bucks builds the gateway's ApprovalRegistry — which needs a Sender to
// post keyboards — and hands it here, breaking the construction-order cycle without an
// import cycle (the Approver interface lives in this package; the registry implements
// it). A nil approver is ignored so the channel can never be left with no backend by a
// stray call.
func (t *TelegramChannel) SetApprover(a Approver) {
	if a != nil {
		t.approver = a
	}
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

// RequestApproval delegates to the injected Approver (the gateway's ApprovalRegistry)
// so there is NO second getUpdates poller. The Approver posts the inline Approve/Deny
// keyboard and resolves the operator's tap from the single gateway stream, preserving
// the fail-safe (timeout/error -> Denied). With no Approver wired, it fails SAFE:
// DecisionDenied, never an unapproved above-band trade.
func (t *TelegramChannel) RequestApproval(ctx context.Context, r ApprovalRequest) (Decision, error) {
	if t.approver == nil {
		return DecisionDenied, fmt.Errorf("channel: no approval backend wired — denying above-band trade (fail-safe)")
	}
	return t.approver.RequestApproval(ctx, r)
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
