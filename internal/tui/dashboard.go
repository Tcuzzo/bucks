package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"bucks/internal/channel"
	"bucks/internal/orders"
)

// ChatResponder is the dashboard's tiny chat seam: one method that takes the owner's
// text and returns BUCKS's reply text (or an error). The TUI talks to THIS, never to
// the chat package directly, so the dashboard stays unit-testable with a fake and
// carries no LLM/HTTP handle of its own (the never-stall invariant: the call is run
// inside a tea.Cmd, off the Update loop). A nil ChatResponder means chat is disabled
// (the read-only dashboard, exactly as before).
type ChatResponder interface {
	// Say answers one turn. It may block on a network call — which is WHY the
	// dashboard only ever invokes it inside a tea.Cmd, never inline in Update/View.
	Say(ctx context.Context, text string) (string, error)
}

// who labels a chat transcript line's speaker.
type who int

const (
	whoYou   who = iota // the owner typed it
	whoBucks            // BUCKS replied
	whoSys              // a system hint/error line (not from the model)
)

// chatLine is one rendered line of the in-dashboard conversation.
type chatLine struct {
	who  who
	text string
}

// chatReplyMsg is the tea.Msg a chat Say command returns when the reply (or error)
// arrives. It is delivered to Update off the loop, so the slow LLM call never blocks
// a frame. Exactly one of text/err is meaningful (err non-nil => the call failed).
type chatReplyMsg struct {
	text string
	err  error
}

// Health is the trader's live health surface. It is carried INTO the dashboard by
// the owner of the trade loop (the harness) — the dashboard never computes it, so
// the dashboard's Update stays a pure in-memory state swap (never-stall).
type Health struct {
	// Halted reports whether the kill switch is engaged. A halted trader is the
	// single most important thing on the dashboard and is rendered loud.
	Halted bool
	// HaltReason is the plain-English reason trading is halted (when Halted).
	HaltReason string
	// Backend is the reasoning backend currently in use (e.g. "oauth-gpt"), so the
	// owner can see which brain is steering.
	Backend string
	// Live is true when trading real money, false for paper (the safe state).
	Live bool
	// LastHeartbeat is the trader's clock value at its last liveness pulse; the
	// dashboard derives freshness from it against the snapshot's Now (no wall clock
	// is read inside the model — freshness is computed from injected times only).
	LastHeartbeat time.Time
}

// Snapshot is the immutable trade-state payload delivered to the dashboard as a
// tea.Msg. The harness builds it (a channel.Report plus Health and the reference
// Now) off the Update loop and hands it over; the dashboard just stores and
// renders it. Because it is a value with no handles, applying it can never block.
type Snapshot struct {
	// Now is the reference time the snapshot was taken (the trader's injected
	// clock). Heartbeat freshness is Now - Health.LastHeartbeat; no time.Now() is
	// ever called inside the dashboard.
	Now time.Time
	// Report is the deterministic P&L / positions / rationales bundle (reused from
	// the channel package so the dashboard and the operator report agree exactly).
	Report channel.Report
	// Health is the kill-switch / backend / mode / heartbeat surface.
	Health Health
}

// SnapshotMsg wraps a Snapshot as a tea.Msg so the bubbletea runtime can deliver
// it to the dashboard's Update. The harness sends it via Program.Send (or a
// tea.Cmd), NEVER by calling the model synchronously — that is the whole point of
// modeling slow state arrival as a message rather than an Update-time fetch.
type SnapshotMsg struct{ Snapshot Snapshot }

// HeartbeatStaleAfter is the freshness threshold: a heartbeat older than this is
// shown as STALE. It is a policy constant, not a wall-clock read.
const HeartbeatStaleAfter = 90 * time.Second

// DashboardModel is the live trader dashboard. It implements tea.Model. It holds
// only the latest snapshot (a value) and precomputed styles — no client, socket,
// or file handle — so Update is a pure in-memory swap and never stalls.
type DashboardModel struct {
	snap     Snapshot
	haveSnap bool
	styles   styleSet

	// --- chat surface (nil chat => read-only dashboard, exactly as before) ---
	chat       ChatResponder // nil disables chat (the read-only path)
	transcript []chatLine    // the rendered conversation, oldest first
	input      string        // the line the owner is currently typing
	thinking   bool          // true while awaiting a reply (the Say cmd is in flight)

	settingsRequested bool // true when the outer runner should open AI settings
}

// NewDashboard constructs an empty, READ-ONLY dashboard (no snapshot, no chat). This
// is the unchanged constructor existing callers/tests use — chat is nil, so the key
// map and view are exactly as before (q quits; no input line).
func NewDashboard() DashboardModel {
	return DashboardModel{styles: newStyles()}
}

// NewDashboardWithChat constructs a dashboard with the chat surface ENABLED, wired to
// resp (the owner can type and talk to BUCKS). A nil resp is allowed and degrades to
// the read-only dashboard. This is what launch uses so `bucks` opens chat-ready.
func NewDashboardWithChat(resp ChatResponder) DashboardModel {
	return DashboardModel{styles: newStyles(), chat: resp}
}

// SetChat returns a copy of the model with the chat responder set, so a caller that
// already built a dashboard can enable chat without reconstructing it. Returning a
// value keeps DashboardModel a value type (tea.Model).
func (m DashboardModel) SetChat(resp ChatResponder) DashboardModel {
	m.chat = resp
	return m
}

// chatEnabled reports whether the chat surface is active (a responder is wired).
func (m DashboardModel) chatEnabled() bool { return m.chat != nil }

// Init implements tea.Model. The dashboard performs no startup I/O of its own; the
// harness pushes snapshots in as messages. Returns nil — never-stall from frame 0.
func (m DashboardModel) Init() tea.Cmd { return nil }

// Update implements tea.Model. It applies a SnapshotMsg by STORING it (a value
// copy) and handles quit keys. It does NO network/disk/blocking work: a slow data
// source is the harness's job to fetch and deliver as a SnapshotMsg, never an
// inline call here. Every path returns promptly.
func (m DashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case SnapshotMsg:
		m.snap = msg.Snapshot
		m.haveSnap = true
		return m, nil
	case chatReplyMsg:
		// The async Say finished. Clear the thinking flag and append BUCKS's reply (or
		// an error line — never a crash, never a fabricated answer).
		m.thinking = false
		if msg.err != nil {
			m.transcript = append(m.transcript, chatLine{who: whoSys, text: "error: " + msg.err.Error()})
		} else {
			m.transcript = append(m.transcript, chatLine{who: whoBucks, text: msg.text})
		}
		return m, nil
	case tea.KeyMsg:
		// Settings work happens outside the Bubble Tea loop. Record intent and quit;
		// the outer runner performs encrypted I/O, then rebuilds this model.
		if msg.Type == tea.KeyCtrlS || (!m.chatEnabled() && msg.Type == tea.KeyRunes && strings.EqualFold(string(msg.Runes), "s")) {
			m.settingsRequested = true
			return m, tea.Quit
		}
		if m.chatEnabled() {
			return m.updateChatKey(msg)
		}
		// Read-only dashboard (no chat): the historical key map — q or ctrl+c quits.
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyRunes:
			if strings.EqualFold(string(msg.Runes), "q") {
				return m, tea.Quit
			}
		}
	}
	return m, nil
}

// updateChatKey is the chat-mode key map. Because the owner is TYPING, ordinary runes
// (including 'q') go into the input buffer — quitting is ctrl+c (always) or esc when
// the input is empty. Enter sends the line. Every path returns promptly; the only
// slow work (the model call) is deferred into a tea.Cmd, never run inline here.
func (m DashboardModel) updateChatKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyEsc:
		// Esc quits only on an empty input (a clean exit); with text typed it clears the
		// line instead of quitting, so esc mid-message never drops the owner out.
		if m.input == "" {
			return m, tea.Quit
		}
		m.input = ""
		return m, nil
	case tea.KeyEnter:
		return m.submitChat()
	case tea.KeyBackspace:
		if r := []rune(m.input); len(r) > 0 {
			m.input = string(r[:len(r)-1])
		}
		return m, nil
	case tea.KeyRunes:
		m.input += string(msg.Runes)
		return m, nil
	case tea.KeySpace:
		// Some terminals deliver space as its own key type rather than a rune.
		m.input += " "
		return m, nil
	}
	return m, nil
}

// submitChat handles Enter: if there's text and a responder, push the owner's line,
// flip to thinking, clear the input, and return the async Say command. With no
// responder it pushes a one-line hint instead of calling (chat is unavailable). An
// empty input is a no-op. NOTHING blocks here — the model call is the returned cmd.
func (m DashboardModel) submitChat() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input)
	if text == "" {
		return m, nil
	}
	if m.chat == nil {
		// Defensive: chat-mode key map only runs when enabled, but never crash.
		m.transcript = append(m.transcript, chatLine{who: whoSys, text: chatHintLine})
		m.input = ""
		return m, nil
	}
	m.transcript = append(m.transcript, chatLine{who: whoYou, text: text})
	m.input = ""
	m.thinking = true
	resp := m.chat
	// The async LLM call as a tea.Cmd: bubbletea runs it OFF the Update loop and
	// delivers the result back as a chatReplyMsg. Update/View never block on it.
	return m, func() tea.Msg {
		out, err := resp.Say(context.Background(), text)
		return chatReplyMsg{text: out, err: err}
	}
}

// Snapshot returns the latest stored snapshot (for tests).
func (m DashboardModel) CurrentSnapshot() (Snapshot, bool) { return m.snap, m.haveSnap }

// SettingsRequested tells the outer dashboard runner to open the AI settings flow.
func (m DashboardModel) SettingsRequested() bool { return m.settingsRequested }

// View implements tea.Model. Pure render of the latest snapshot — no I/O, no
// blocking — so it is safe every frame and fully assertable. All money is rendered
// from orders.Decimal via String() (never float64).
func (m DashboardModel) View() string {
	var b strings.Builder
	// The BUCKS wordmark sits UP TOP on every dashboard frame — the brand mark leads, then a
	// thin locator line. RenderBanner is a pure colored-string transform (no I/O), safe to
	// render every frame, and keeps the never-stall invariant.
	b.WriteString(RenderBanner())
	b.WriteString("\n")
	b.WriteString(m.styles.header.Render("$ BUCKS — live dashboard"))
	b.WriteString("\n\n")

	if !m.haveSnap {
		b.WriteString(m.styles.hint.Render("waiting for first snapshot…\n"))
		// Even before the first snapshot, the chat surface (or its hint) is shown so
		// the owner can start talking / sees how to enable chat right away.
		b.WriteString(m.chatView())
		return b.String()
	}

	if m.chatEnabled() {
		// Chat-on: a COMPACT health/account summary keeps the conversation in view.
		b.WriteString(m.compactSummary())
	} else {
		// Read-only: the full health + account + positions surface (unchanged).
		b.WriteString(m.fullSnapshotView())
	}

	b.WriteString(m.chatView())
	return b.String()
}

// fullSnapshotView is the original read-only render: the loud health block, the
// account P&L, and every open position. Used when chat is disabled.
func (m DashboardModel) fullSnapshotView() string {
	var b strings.Builder
	// --- Health surface (most important; rendered first and loud) ---
	b.WriteString(m.healthView())
	b.WriteString("\n")

	// --- Account P&L (exact decimal text) ---
	rep := m.snap.Report
	b.WriteString(m.styles.prompt.Render("Account"))
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("  Equity:        %s\n", money(rep.Equity)))
	b.WriteString(fmt.Sprintf("  Realized P&L:  %s\n", signedMoney(m, rep.RealizedPL)))
	b.WriteString(fmt.Sprintf("  Unrealized P&L:%s\n", " "+signedMoney(m, rep.UnrealizedPL)))
	b.WriteString("\n")

	// --- Positions (symbol, qty, P&L as Decimal) ---
	b.WriteString(m.styles.prompt.Render("Positions"))
	b.WriteString("\n")
	if len(rep.Positions) == 0 {
		b.WriteString(m.styles.hint.Render("  (flat — no open positions)\n"))
	} else {
		for _, p := range rep.Positions {
			b.WriteString(fmt.Sprintf("  %-8s qty %-10s mark %-10s  P&L %s\n",
				p.Symbol, money(p.Qty), money(p.MarkPx), signedMoney(m, p.UnrealizedPL)))
		}
	}
	return b.String()
}

// compactSummary is the trimmed health/account line shown above the chat when chat is
// on, so the conversation has room while the owner still sees status + money at a
// glance. Status (HALTED/LIVE), mode, exact equity, and exact P&L are all kept — the
// money is still rendered via Decimal.String() (never float64).
func (m DashboardModel) compactSummary() string {
	var b strings.Builder
	h := m.snap.Health
	rep := m.snap.Report
	status := m.styles.good.Render("LIVE")
	if h.Halted {
		reason := h.HaltReason
		if reason == "" {
			reason = "kill switch engaged"
		}
		status = m.styles.bad.Render("HALTED — " + reason)
	}
	b.WriteString(m.styles.prompt.Render("Status"))
	b.WriteString("  " + status + "   " + modeLabel(h.Live) + "\n")
	b.WriteString(fmt.Sprintf("  Equity %s   Realized %s   Unrealized %s\n",
		money(rep.Equity), signedMoney(m, rep.RealizedPL), signedMoney(m, rep.UnrealizedPL)))
	if len(rep.Positions) == 0 {
		b.WriteString(m.styles.hint.Render("  flat — no open positions\n"))
	} else {
		b.WriteString(m.styles.hint.Render(fmt.Sprintf("  %d open position(s)\n", len(rep.Positions))))
	}
	b.WriteString("\n")
	return b.String()
}

// chatHintLine is the one-line, plain-English nudge shown when no chat backend is
// configured: it names the FREE Nemotron path first so a key-less owner has an
// immediate way to turn chat on.
const chatHintLine = "AI is not configured — press s to set it up, or run `bucks settings`. Free NVIDIA Nemotron works with a no-credit-card key."

// chatView renders the conversation transcript, a thinking indicator while a reply is
// in flight, and the input line — OR, when chat is disabled, a single hint line. It is
// a pure string transform (no I/O), safe every frame.
func (m DashboardModel) chatView() string {
	var b strings.Builder
	b.WriteString(m.styles.prompt.Render("Chat"))
	b.WriteString("\n")

	if !m.chatEnabled() {
		b.WriteString(m.styles.hint.Render("  " + chatHintLine + "\n"))
		return b.String()
	}

	for _, ln := range m.transcript {
		switch ln.who {
		case whoYou:
			b.WriteString("  " + m.styles.prompt.Render("you") + " › " + ln.text + "\n")
		case whoBucks:
			b.WriteString("  " + m.styles.good.Render("bucks") + " › " + ln.text + "\n")
		default: // whoSys
			b.WriteString("  " + m.styles.hint.Render(ln.text) + "\n")
		}
	}
	if m.thinking {
		b.WriteString("  " + m.styles.hint.Render("thinking…") + "\n")
	}
	// The input line with a block cursor so the owner sees where they're typing.
	b.WriteString("  " + m.styles.prompt.Render("you") + " › " + m.input + "▌\n")
	b.WriteString("  " + m.styles.hint.Render("[ctrl+s] AI settings") + "\n")
	return b.String()
}

// healthView renders the kill-switch / backend / mode / heartbeat block. A halted
// trader is surfaced in red and unmistakable; a live (running) trader in green.
func (m DashboardModel) healthView() string {
	var b strings.Builder
	h := m.snap.Health
	b.WriteString(m.styles.prompt.Render("Health"))
	b.WriteString("\n")
	if h.Halted {
		reason := h.HaltReason
		if reason == "" {
			reason = "kill switch engaged"
		}
		b.WriteString("  Status:  " + m.styles.bad.Render("HALTED — "+reason) + "\n")
	} else {
		b.WriteString("  Status:  " + m.styles.good.Render("LIVE — trading active") + "\n")
	}
	b.WriteString(fmt.Sprintf("  Mode:    %s\n", modeLabel(h.Live)))
	backend := h.Backend
	if backend == "" {
		backend = "(none)"
	}
	b.WriteString(fmt.Sprintf("  Backend: %s\n", backend))
	b.WriteString("  " + m.heartbeatLine() + "\n")
	return b.String()
}

// heartbeatLine derives freshness from the snapshot's Now minus the last heartbeat
// — purely from injected times, no wall clock. A stale or missing heartbeat is
// flagged so the owner sees a stalled loop immediately.
func (m DashboardModel) heartbeatLine() string {
	h := m.snap.Health
	if h.LastHeartbeat.IsZero() {
		return "Heartbeat: " + m.styles.bad.Render("none yet")
	}
	age := m.snap.Now.Sub(h.LastHeartbeat)
	if age < 0 {
		age = 0
	}
	if age > HeartbeatStaleAfter {
		return "Heartbeat: " + m.styles.bad.Render(fmt.Sprintf("STALE (%s ago)", age.Round(time.Second)))
	}
	return "Heartbeat: " + m.styles.good.Render(fmt.Sprintf("fresh (%s ago)", age.Round(time.Second)))
}

// money renders a Decimal as its exact string (never float64).
func money(d orders.Decimal) string { return d.String() }

// signedMoney renders a Decimal with a +/- tint so gains/losses read at a glance,
// still as exact decimal text. The sign and value come from the Decimal itself.
func signedMoney(m DashboardModel, d orders.Decimal) string {
	s := d.String()
	switch d.Sign() {
	case 1:
		return m.styles.good.Render("+" + s)
	case -1:
		return m.styles.bad.Render(s) // String() already carries the '-'
	default:
		return m.styles.dim.Render(s)
	}
}

// compile-time assertion that DashboardModel satisfies tea.Model.
var _ tea.Model = DashboardModel{}
