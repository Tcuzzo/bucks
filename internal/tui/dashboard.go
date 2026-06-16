package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"bucks/internal/channel"
	"bucks/internal/orders"
)

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
}

// NewDashboard constructs an empty dashboard (no snapshot yet).
func NewDashboard() DashboardModel {
	return DashboardModel{styles: newStyles()}
}

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
	case tea.KeyMsg:
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

// Snapshot returns the latest stored snapshot (for tests).
func (m DashboardModel) CurrentSnapshot() (Snapshot, bool) { return m.snap, m.haveSnap }

// View implements tea.Model. Pure render of the latest snapshot — no I/O, no
// blocking — so it is safe every frame and fully assertable. All money is rendered
// from orders.Decimal via String() (never float64).
func (m DashboardModel) View() string {
	var b strings.Builder
	b.WriteString(m.styles.header.Render("$ BUCKS — live dashboard"))
	b.WriteString("\n\n")

	if !m.haveSnap {
		b.WriteString(m.styles.hint.Render("waiting for first snapshot…\n"))
		return b.String()
	}

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
