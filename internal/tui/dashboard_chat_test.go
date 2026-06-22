package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"bucks/internal/channel"
)

// fakeResponder is a hermetic ChatResponder: it records the text it was asked and
// returns a canned reply or error. It makes NO network call, so the dashboard chat
// model is fully unit-testable with zero real LLM.
type fakeResponder struct {
	reply string
	err   error
	asked string
}

func (f *fakeResponder) Say(_ context.Context, text string) (string, error) {
	f.asked = text
	if f.err != nil {
		return "", f.err
	}
	return f.reply, nil
}

// sendChatDash drives one message through the dashboard and returns the concrete
// type + cmd (mirrors sendDash but used by the chat tests).
func sendChatDash(t *testing.T, m DashboardModel, msg tea.Msg) (DashboardModel, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	dm, ok := next.(DashboardModel)
	if !ok {
		t.Fatalf("Update returned %T, want DashboardModel", next)
	}
	return dm, cmd
}

// TestDashboardChatTypingBuildsInput proves typing runes (including 'q') builds the
// input buffer when a ChatResponder is set, and 'q' does NOT quit while typing.
func TestDashboardChatTypingBuildsInput(t *testing.T) {
	resp := &fakeResponder{reply: "hey"}
	m := NewDashboardWithChat(resp)

	// Type "qty" — note the leading 'q' must be INSERTED, not treated as quit.
	for _, r := range "qty" {
		var cmd tea.Cmd
		m, cmd = sendChatDash(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		if cmd != nil {
			t.Fatalf("typing %q returned a non-nil cmd (should not quit/act): %v", r, cmd)
		}
	}
	v := m.View()
	if !strings.Contains(v, "qty") {
		t.Errorf("typed input %q not reflected in the input line:\n%s", "qty", v)
	}
}

// TestDashboardChatBackspaceTrims proves backspace trims the last rune.
func TestDashboardChatBackspaceTrims(t *testing.T) {
	m := NewDashboardWithChat(&fakeResponder{reply: "ok"})
	for _, r := range "hi" {
		m, _ = sendChatDash(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m, _ = sendChatDash(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	v := m.View()
	if strings.Contains(v, "hi") {
		t.Errorf("backspace did not trim — still shows full input:\n%s", v)
	}
	if !strings.Contains(v, "h") {
		t.Errorf("backspace over-trimmed — 'h' missing:\n%s", v)
	}
}

// TestDashboardChatEnterAsksAndAppendsReply proves Enter with a fake responder pushes
// the user line + returns a tea.Cmd (the async Say), and that feeding the resulting
// chatReplyMsg appends BUCKS's reply. The model NEVER blocks on the LLM.
func TestDashboardChatEnterAsksAndAppendsReply(t *testing.T) {
	resp := &fakeResponder{reply: "positions look flat"}
	m := NewDashboardWithChat(resp)
	for _, r := range "how am i doing" {
		m, _ = sendChatDash(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m, cmd := sendChatDash(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter with a responder must return a tea.Cmd (the async Say), got nil")
	}
	// The user line is pushed immediately; the input is cleared; thinking shows.
	v := m.View()
	if !strings.Contains(v, "how am i doing") {
		t.Errorf("user turn not pushed to transcript:\n%s", v)
	}
	if !strings.Contains(strings.ToLower(v), "thinking") {
		t.Errorf("thinking indicator not shown while awaiting reply:\n%s", v)
	}

	// Run the cmd (this is what bubbletea does off the Update loop) to get the reply
	// msg, then feed it back — proving Say runs as a command, not inline in Update.
	msg := cmd()
	reply, ok := msg.(chatReplyMsg)
	if !ok {
		t.Fatalf("cmd returned %T, want chatReplyMsg", msg)
	}
	if resp.asked != "how am i doing" {
		t.Errorf("responder asked %q, want %q", resp.asked, "how am i doing")
	}
	m, _ = sendChatDash(t, m, reply)
	v = m.View()
	if !strings.Contains(v, "positions look flat") {
		t.Errorf("BUCKS reply not appended to transcript:\n%s", v)
	}
	if strings.Contains(strings.ToLower(v), "thinking") {
		t.Errorf("thinking indicator still shown after reply arrived:\n%s", v)
	}
}

// TestDashboardChatErrorReplyShowsErrorLine proves an error reply renders an error
// line and never panics.
func TestDashboardChatErrorReplyShowsErrorLine(t *testing.T) {
	m := NewDashboardWithChat(&fakeResponder{err: errors.New("backend down")})
	for _, r := range "hi" {
		m, _ = sendChatDash(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	_, cmd := sendChatDash(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter must return a cmd even when the backend will error")
	}
	msg := cmd()
	reply, ok := msg.(chatReplyMsg)
	if !ok {
		t.Fatalf("cmd returned %T, want chatReplyMsg", msg)
	}
	if reply.err == nil {
		t.Fatal("expected the chatReplyMsg to carry the backend error")
	}
	m2, _ := sendChatDash(t, m, reply)
	v := m2.View()
	if !strings.Contains(strings.ToLower(v), "error") && !strings.Contains(v, "backend down") {
		t.Errorf("error reply not surfaced as an error line:\n%s", v)
	}
}

// TestDashboardChatCtrlCQuits proves ctrl+c always quits, even with chat on.
func TestDashboardChatCtrlCQuits(t *testing.T) {
	m := NewDashboardWithChat(&fakeResponder{reply: "ok"})
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
		t.Error("ctrl+c must quit even when chat is enabled")
	}
}

// TestDashboardChatEscQuitsWhenInputEmpty proves esc quits only when the input is
// empty (so esc is a clean exit, not a destructive keystroke mid-message).
func TestDashboardChatEscQuitsWhenInputEmpty(t *testing.T) {
	m := NewDashboardWithChat(&fakeResponder{reply: "ok"})
	// Empty input: esc quits.
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc}); cmd == nil {
		t.Error("esc with empty input should quit")
	}
	// Non-empty input: esc does NOT quit (it clears, never crashes).
	m2 := NewDashboardWithChat(&fakeResponder{reply: "ok"})
	m2, _ = sendChatDash(t, m2, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if _, cmd := m2.Update(tea.KeyMsg{Type: tea.KeyEsc}); cmd != nil {
		t.Error("esc with non-empty input must NOT quit")
	}
}

// TestDashboardNilResponderShowsHint proves a nil ChatResponder disables chat: the
// View surfaces the configure-backend hint (mentioning the free Nemotron path) so the
// owner knows chat is one step away, and the read-only quit keys still work (q quits,
// Enter does nothing/never crashes).
func TestDashboardNilResponderShowsHint(t *testing.T) {
	m := NewDashboard() // no chat -> read-only as today
	v := m.View()
	if !strings.Contains(strings.ToLower(v), "nemotron") {
		t.Errorf("no-backend hint should mention the free Nemotron path:\n%s", v)
	}

	// Enter with no responder must not return an action cmd and must not crash.
	m2, cmd := sendChatDash(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Errorf("Enter with no responder must not return an action cmd, got %v", cmd)
	}
	_ = m2.View() // no panic

	// The read-only key map is intact: q still quits when chat is disabled.
	if _, qcmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}); qcmd == nil {
		t.Error("q must still quit the read-only dashboard")
	}
}

// TestDashboardChatStillRendersHealth proves the chat surface does NOT erase the
// health/positions summary — a snapshot still renders alongside the chat input.
func TestDashboardChatStillRendersHealth(t *testing.T) {
	m := NewDashboardWithChat(&fakeResponder{reply: "ok"})
	m, _ = sendChatDash(t, m, SnapshotMsg{Snapshot: Snapshot{
		Report: channel.Report{Equity: dec("12345.67"), RealizedPL: dec("0"), UnrealizedPL: dec("0")},
		Health: Health{Backend: "nvidia"},
	}})
	v := m.View()
	if !strings.Contains(v, "BUCKS") {
		t.Errorf("brand header missing with chat on:\n%s", v)
	}
	if !strings.Contains(v, "12345.67") {
		t.Errorf("health/account summary erased by chat surface:\n%s", v)
	}
	if !strings.Contains(strings.ToLower(v), "you") {
		t.Errorf("chat input line missing with chat on:\n%s", v)
	}
}

func TestDashboardWithoutAIAdvertisesSettings(t *testing.T) {
	m := NewDashboard()
	m, _ = sendChatDash(t, m, SnapshotMsg{Snapshot: Snapshot{
		Report: channel.Report{Equity: dec("10000"), RealizedPL: dec("0"), UnrealizedPL: dec("0")},
	}})
	view := strings.ToLower(m.View())
	for _, want := range []string{"press s", "bucks settings"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing-AI dashboard does not contain %q:\n%s", want, m.View())
		}
	}
}

func TestDashboardSRequestsSettingsWhenChatUnavailable(t *testing.T) {
	m := NewDashboard()
	next, cmd := sendChatDash(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if cmd == nil {
		t.Fatal("s should quit the dashboard into settings")
	}
	if !next.SettingsRequested() {
		t.Fatal("s did not record a settings request")
	}
}

func TestDashboardCtrlSRequestsSettingsWhileChatIsActive(t *testing.T) {
	m := NewDashboardWithChat(&fakeResponder{reply: "ok"})
	next, cmd := sendChatDash(t, m, tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd == nil {
		t.Fatal("ctrl+s should quit the dashboard into settings")
	}
	if !next.SettingsRequested() {
		t.Fatal("ctrl+s did not record a settings request")
	}
}
