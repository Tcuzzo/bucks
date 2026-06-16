package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"bucks/internal/channel"
	"bucks/internal/orders"
)

// sendDash drives one message through the dashboard and returns the concrete type.
func sendDash(t *testing.T, m DashboardModel, msg tea.Msg) (DashboardModel, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	dm, ok := next.(DashboardModel)
	if !ok {
		t.Fatalf("Update returned %T, want DashboardModel", next)
	}
	return dm, cmd
}

// dec parses a decimal for tests (test-only literals, MustParse is fine).
func dec(s string) orders.Decimal { return orders.MustParseDecimal(s) }

// TestDashboardRendersSnapshot feeds a snapshot and asserts the View contains the
// positions, the EXACT decimal P&L text, and the health state.
func TestDashboardRendersSnapshot(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	snap := Snapshot{
		Now: now,
		Report: channel.Report{
			GeneratedAt:  now,
			Equity:       dec("25431.50"),
			RealizedPL:   dec("120.25"),
			UnrealizedPL: dec("-45.75"),
			Positions: []channel.Position{
				{Symbol: "AAPL", Qty: dec("10"), MarkPx: dec("195.40"), UnrealizedPL: dec("88.00")},
				{Symbol: "TSLA", Qty: dec("-5"), MarkPx: dec("210.10"), UnrealizedPL: dec("-133.75")},
			},
		},
		Health: Health{
			Halted:        false,
			Backend:       "oauth-gpt",
			Live:          false,
			LastHeartbeat: now.Add(-10 * time.Second),
		},
	}

	m := NewDashboard()
	m, _ = sendDash(t, m, SnapshotMsg{Snapshot: snap})
	got, ok := m.CurrentSnapshot()
	if !ok {
		t.Fatal("dashboard did not record the snapshot")
	}
	if got.Report.Equity.String() != "25431.50" {
		t.Fatalf("stored equity = %s", got.Report.Equity.String())
	}

	v := m.View()

	// Positions present by symbol.
	for _, sym := range []string{"AAPL", "TSLA"} {
		if !strings.Contains(v, sym) {
			t.Errorf("view missing position %q", sym)
		}
	}
	// EXACT decimal P&L text (never float64 formatting like 120.250000001).
	for _, want := range []string{"25431.50", "120.25", "-45.75", "88.00", "-133.75"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing exact decimal %q\n---\n%s", want, v)
		}
	}
	// Health: a running trader is surfaced as LIVE/active and PAPER mode.
	if !strings.Contains(v, "LIVE") {
		t.Errorf("running trader not surfaced as LIVE/active:\n%s", v)
	}
	if !strings.Contains(v, "PAPER") {
		t.Errorf("paper mode not surfaced:\n%s", v)
	}
	if !strings.Contains(v, "oauth-gpt") {
		t.Errorf("backend not surfaced:\n%s", v)
	}
	if !strings.Contains(strings.ToLower(v), "fresh") {
		t.Errorf("fresh heartbeat not surfaced:\n%s", v)
	}
}

// TestDashboardSurfacesHalt proves a halted state is clear and unmistakable.
func TestDashboardSurfacesHalt(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	snap := Snapshot{
		Now: now,
		Report: channel.Report{
			Equity:       dec("9000"),
			RealizedPL:   dec("-1000"),
			UnrealizedPL: dec("0"),
		},
		Health: Health{
			Halted:        true,
			HaltReason:    "daily-loss circuit breaker tripped",
			Backend:       "cloud-key",
			Live:          true,
			LastHeartbeat: now.Add(-5 * time.Minute), // stale
		},
	}
	m := NewDashboard()
	m, _ = sendDash(t, m, SnapshotMsg{Snapshot: snap})
	v := m.View()

	if !strings.Contains(v, "HALTED") {
		t.Errorf("halted state not surfaced:\n%s", v)
	}
	if !strings.Contains(v, "daily-loss circuit breaker tripped") {
		t.Errorf("halt reason not surfaced:\n%s", v)
	}
	// A live trader at halt should still show LIVE mode (real money was armed).
	if !strings.Contains(v, "LIVE (real money)") {
		t.Errorf("live mode label not surfaced:\n%s", v)
	}
	// A 5-minute-old heartbeat is past the 90s threshold => STALE.
	if !strings.Contains(v, "STALE") {
		t.Errorf("stale heartbeat not flagged:\n%s", v)
	}
}

// TestDashboardEmptyBeforeSnapshot proves the dashboard renders a waiting state
// (never a crash / panic) before the first snapshot arrives.
func TestDashboardEmptyBeforeSnapshot(t *testing.T) {
	m := NewDashboard()
	if _, ok := m.CurrentSnapshot(); ok {
		t.Fatal("fresh dashboard reports a snapshot")
	}
	v := m.View()
	if !strings.Contains(v, "BUCKS") {
		t.Errorf("empty dashboard missing brand header:\n%s", v)
	}
	if !strings.Contains(strings.ToLower(v), "waiting") {
		t.Errorf("empty dashboard missing waiting state:\n%s", v)
	}
}

// TestDashboardFlatPositions proves the flat (no positions) case renders cleanly.
func TestDashboardFlatPositions(t *testing.T) {
	now := time.Now()
	snap := Snapshot{
		Now:    now,
		Report: channel.Report{Equity: dec("10000"), RealizedPL: dec("0"), UnrealizedPL: dec("0")},
		Health: Health{LastHeartbeat: now},
	}
	m := NewDashboard()
	m, _ = sendDash(t, m, SnapshotMsg{Snapshot: snap})
	v := m.View()
	if !strings.Contains(strings.ToLower(v), "flat") {
		t.Errorf("flat positions state not surfaced:\n%s", v)
	}
}

// TestDashboardQuitKeys proves q and ctrl+c return a quit command.
func TestDashboardQuitKeys(t *testing.T) {
	m := NewDashboard()
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
		t.Error("ctrl+c should quit the dashboard")
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}); cmd == nil {
		t.Error("q should quit the dashboard")
	}
}
