package gateway

// commands.go — the inbound command router. It is the Handler that turns an
// operator's text command (/status, /summary, /positions, /halt, /resume, /help)
// into an action on BUCKS plus a plain-English reply.
//
// Two laws shape this file:
//
//   - OPERATOR AUTHORITY. Only the operator's own chat may command BUCKS. Any
//     update from a different chat id is IGNORED entirely — no reply, no action.
//     This is the operator-authority lesson: a public/untrusted surface cannot
//     drive a tool that DOES anything.
//
//   - SINGLE OWNER, NARROW SCOPE. The gateway is the only poller; THIS router only
//     acts on TEXT commands. A callback_query (an inline-button tap, Message nil)
//     belongs to a DIFFERENT handler — the approval registry in a later slice — so
//     this router must NOT consume it. Consuming it here would steal taps from the
//     approval flow.
//
// Every external thing the handlers touch is behind an interface (CommandContext,
// Sender) so tests inject fakes: no real kill switch, no real trader, no network.

import (
	"context"
	"fmt"
	"strings"

	"bucks/internal/channel"
)

// StatusInfo is the at-a-glance state /status renders. Equity is pre-formatted as
// a string by the producer (the live wiring renders the Decimal with .String());
// keeping it a string here keeps this router free of the money type and float-free.
type StatusInfo struct {
	Halted     bool
	HaltReason string
	Mode       string // "paper" or "live"
	Broker     string
	Equity     string
}

// CommandContext is the seam the command handlers act on. The live wiring backs
// this with the durable kill switch and the trader's report builder; tests back it
// with a scripted fake. None of these methods touch Telegram — replies go out via
// Sender, keeping the action and the transport decoupled.
type CommandContext interface {
	// Halt trips the durable, manual kill switch (operator-initiated).
	Halt(reason string) error
	// Resume clears the kill switch (the only exit from a halt).
	Resume() error
	// Status returns the at-a-glance trading state for /status.
	Status() StatusInfo
	// Report returns the current P&L / position snapshot for /summary and /positions.
	Report() channel.Report
}

// Sender delivers a reply to a chat. The live wiring wraps Telegram sendMessage;
// tests use a recorder. The router never imports the concrete channel transport.
type Sender interface {
	Send(ctx context.Context, chatID int64, text string) error
}

// haltReason is the fixed reason recorded when the operator runs /halt, so the
// audit trail (and a later /status) shows the halt came from a Telegram command.
const haltReason = "operator /halt via Telegram"

// CommandRouter is the Handler that processes inbound operator text commands. It is
// gated to a single trusted chat id.
type CommandRouter struct {
	trustedChatID int64
	cmd           CommandContext
	send          Sender
	logf          func(string, ...any)
}

// CommandOption configures a CommandRouter.
type CommandOption func(*CommandRouter)

// WithCommandLogger injects a log sink (a failed Send is logged, not fatal). A nil
// func is ignored.
func WithCommandLogger(logf func(string, ...any)) CommandOption {
	return func(r *CommandRouter) {
		if logf != nil {
			r.logf = logf
		}
	}
}

// NewCommandRouter builds a router that only obeys commands from trustedChatID,
// acting on cmd and replying via send.
func NewCommandRouter(trustedChatID int64, cmd CommandContext, send Sender, opts ...CommandOption) *CommandRouter {
	r := &CommandRouter{
		trustedChatID: trustedChatID,
		cmd:           cmd,
		send:          send,
		logf:          func(string, ...any) {}, // silent by default
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Handle dispatches one update. It ignores anything that is not a text message
// from the trusted chat: callbacks (Message nil) belong to the approval handler,
// empty text is noise, and a non-trusted chat is silently dropped (operator
// authority). It never panics on malformed input.
func (r *CommandRouter) Handle(ctx context.Context, u Update) {
	// Not a text message (e.g. a callback_query, or a message with no text): this
	// router does not act on it and does not consume it.
	if u.Message == nil || u.Message.Text == "" {
		return
	}
	// Fail CLOSED: an unconfigured trusted chat id (0) trusts NO ONE. A safety gate
	// must never fail open when misconfigured — otherwise a chat id of 0 (e.g. an
	// empty/unparsed config) would match every 0-valued update and trust everyone.
	if r.trustedChatID == 0 {
		return
	}
	// Operator authority: only the operator's own chat may command BUCKS. Any other
	// chat is ignored entirely — no reply, no action.
	if u.Message.Chat.ID != r.trustedChatID {
		return
	}

	chatID := u.Message.Chat.ID
	command := parseCommand(u.Message.Text)

	switch command {
	case "/status":
		r.reply(ctx, chatID, r.statusText())
	case "/summary":
		r.reply(ctx, chatID, r.summaryText())
	case "/positions":
		r.reply(ctx, chatID, r.positionsText())
	case "/halt":
		r.handleHalt(ctx, chatID)
	case "/resume":
		r.handleResume(ctx, chatID)
	default:
		// /help, /start, and any unknown command (and plain text, which parses to
		// an empty command) all get the command list.
		r.reply(ctx, chatID, helpText())
	}
}

// parseCommand extracts the leading command token from a message: it takes the
// first whitespace-separated field, strips a trailing "@botname" mention, and
// lowercases it. It returns a normalized "/word" for a slash command, or "" for
// anything that is not a leading slash command (plain text) — which Handle routes
// to help. It never panics.
func parseCommand(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	token := fields[0]
	if !strings.HasPrefix(token, "/") {
		return "" // plain text, not a command
	}
	// Drop a "@botname" mention (Telegram appends it in group chats).
	if at := strings.IndexByte(token, '@'); at >= 0 {
		token = token[:at]
	}
	return strings.ToLower(token)
}

// statusText renders the plain-English /status line.
func (r *CommandRouter) statusText() string {
	s := r.cmd.Status()
	var b strings.Builder
	fmt.Fprintf(&b, "BUCKS status\n")
	fmt.Fprintf(&b, "Mode: %s\n", orUnknown(s.Mode))
	fmt.Fprintf(&b, "Broker: %s\n", orUnknown(s.Broker))
	fmt.Fprintf(&b, "Equity: %s\n", orUnknown(s.Equity))
	if s.Halted {
		reason := s.HaltReason
		if reason == "" {
			reason = "(no reason recorded)"
		}
		fmt.Fprintf(&b, "🛑 HALTED — %s", reason)
	} else {
		b.WriteString("✅ Trading active")
	}
	return b.String()
}

// summaryText renders the plain-English /summary P&L line.
func (r *CommandRouter) summaryText() string {
	rep := r.cmd.Report()
	var b strings.Builder
	b.WriteString("BUCKS P&L summary\n")
	fmt.Fprintf(&b, "Equity: %s\n", rep.Equity.String())
	fmt.Fprintf(&b, "Realized P&L: %s\n", rep.RealizedPL.String())
	fmt.Fprintf(&b, "Unrealized P&L: %s\n", rep.UnrealizedPL.String())
	fmt.Fprintf(&b, "Open positions: %d", len(rep.Positions))
	return b.String()
}

// positionsText renders the plain-English /positions list.
func (r *CommandRouter) positionsText() string {
	rep := r.cmd.Report()
	if len(rep.Positions) == 0 {
		return "No open positions."
	}
	var b strings.Builder
	b.WriteString("Open positions\n")
	for i, p := range rep.Positions {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s: qty %s @ %s, unrealized %s",
			p.Symbol, p.Qty.String(), p.MarkPx.String(), p.UnrealizedPL.String())
	}
	return b.String()
}

// handleHalt trips the kill switch and reports the outcome in plain English. On a
// halt error we must NOT claim halted — we report the failure so the operator
// knows trading was NOT stopped.
func (r *CommandRouter) handleHalt(ctx context.Context, chatID int64) {
	if err := r.cmd.Halt(haltReason); err != nil {
		r.reply(ctx, chatID, fmt.Sprintf("Could not halt trading: %v", err))
		return
	}
	r.reply(ctx, chatID, "🛑 TRADING HALTED — send /resume to clear.")
}

// handleResume clears the kill switch and reports the outcome.
func (r *CommandRouter) handleResume(ctx context.Context, chatID int64) {
	if err := r.cmd.Resume(); err != nil {
		r.reply(ctx, chatID, fmt.Sprintf("Could not resume trading: %v", err))
		return
	}
	r.reply(ctx, chatID, "✅ Trading resumed.")
}

// helpText is the short command list shown for /help, /start, an unknown command,
// or a plain-text message from the operator.
func helpText() string {
	return strings.Join([]string{
		"BUCKS commands:",
		"/status — trading mode, broker, equity, and halt state",
		"/summary — equity and realized/unrealized P&L",
		"/positions — your open positions",
		"/halt — stop all trading now",
		"/resume — resume trading after a halt",
		"/help — show this list",
	}, "\n")
}

// reply sends text to chatID, logging (never crashing) on a transport failure.
func (r *CommandRouter) reply(ctx context.Context, chatID int64, text string) {
	if err := r.send.Send(ctx, chatID, text); err != nil {
		r.logf("gateway: failed to send command reply to chat %d: %v", chatID, err)
	}
}

// orUnknown renders an empty field as "unknown" so a half-populated status never
// shows a blank the operator can't read.
func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
