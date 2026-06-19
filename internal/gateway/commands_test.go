package gateway

import (
	"context"
	"strings"
	"sync"
	"testing"

	"bucks/internal/channel"
	"bucks/internal/orders"
)

// fakeCommandContext is a scripted CommandContext: it records whether Halt/Resume
// were called and returns canned Status/Report snapshots. No real kill switch or
// trader is needed, which is the whole point of the seam.
type fakeCommandContext struct {
	mu sync.Mutex

	haltCalled   bool
	haltReason   string
	resumeCalled bool

	haltErr   error
	resumeErr error

	status StatusInfo
	report channel.Report
}

func (f *fakeCommandContext) Halt(reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.haltCalled = true
	f.haltReason = reason
	return f.haltErr
}

func (f *fakeCommandContext) Resume() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCalled = true
	return f.resumeErr
}

func (f *fakeCommandContext) Status() StatusInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeCommandContext) Report() channel.Report {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.report
}

func (f *fakeCommandContext) didHalt() (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.haltCalled, f.haltReason
}

func (f *fakeCommandContext) didResume() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resumeCalled
}

// fakeSender records every reply (chatID + text) so a test can assert exactly what
// BUCKS said and to whom. No network.
type fakeSender struct {
	mu    sync.Mutex
	sends []sentMessage
}

type sentMessage struct {
	chatID int64
	text   string
}

func (s *fakeSender) Send(_ context.Context, chatID int64, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends = append(s.sends, sentMessage{chatID: chatID, text: text})
	return nil
}

func (s *fakeSender) snapshot() []sentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sentMessage, len(s.sends))
	copy(out, s.sends)
	return out
}

func (s *fakeSender) lastText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sends) == 0 {
		return ""
	}
	return s.sends[len(s.sends)-1].text
}

func (s *fakeSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sends)
}

const testTrustedChat = int64(4242)

// textUpdate builds an Update carrying a text message from chatID.
func textUpdate(chatID int64, text string) Update {
	m := &Message{Text: text, MessageID: 7}
	m.Chat.ID = chatID
	return Update{UpdateID: 1, Message: m}
}

func newRouterFixture() (*CommandRouter, *fakeCommandContext, *fakeSender) {
	cmd := &fakeCommandContext{}
	send := &fakeSender{}
	r := NewCommandRouter(testTrustedChat, cmd, send)
	return r, cmd, send
}

// TestZeroTrustedChatTrustsNoOne pins the fail-CLOSED posture: a router built with an
// unconfigured trusted chat id (0) must trust NO ONE — not even an update that happens
// to arrive from chat id 0. A safety gate must never fail OPEN when misconfigured
// (mirrors the empty-chat-id hard-fail in telegram_live.go).
func TestZeroTrustedChatTrustsNoOne(t *testing.T) {
	cmd := &fakeCommandContext{}
	send := &fakeSender{}
	r := NewCommandRouter(0, cmd, send) // unconfigured trusted chat id

	r.Handle(context.Background(), textUpdate(0, "/halt"))

	if called, _ := cmd.didHalt(); called {
		t.Error("router with trustedChatID==0 halted on a chat-0 message — fail-OPEN; it must trust no one")
	}
	if send.count() != 0 {
		t.Errorf("unconfigured router replied %d times; want 0 (fail-closed, silent)", send.count())
	}
}

// 1. /halt from the trusted chat calls Halt and replies the halted message; /resume calls Resume.
func TestHaltFromTrustedChatCallsHaltAndReplies(t *testing.T) {
	r, cmd, send := newRouterFixture()

	r.Handle(context.Background(), textUpdate(testTrustedChat, "/halt"))

	called, reason := cmd.didHalt()
	if !called {
		t.Fatalf("expected Halt to be called on /halt")
	}
	if reason == "" {
		t.Fatalf("expected a non-empty halt reason passed to Halt, got empty")
	}
	if send.count() != 1 {
		t.Fatalf("expected exactly 1 reply, got %d", send.count())
	}
	if !strings.Contains(send.lastText(), "HALTED") {
		t.Fatalf("expected halted reply, got %q", send.lastText())
	}
}

func TestResumeFromTrustedChatCallsResume(t *testing.T) {
	r, cmd, send := newRouterFixture()

	r.Handle(context.Background(), textUpdate(testTrustedChat, "/resume"))

	if !cmd.didResume() {
		t.Fatalf("expected Resume to be called on /resume")
	}
	if send.count() != 1 {
		t.Fatalf("expected exactly 1 reply, got %d", send.count())
	}
	if !strings.Contains(strings.ToLower(send.lastText()), "resumed") {
		t.Fatalf("expected resumed reply, got %q", send.lastText())
	}
}

// 2. Trusted-chat gate: a /halt from a DIFFERENT chat id does NOT call Halt and sends NO reply.
func TestUntrustedChatIsIgnoredEntirely(t *testing.T) {
	r, cmd, send := newRouterFixture()

	r.Handle(context.Background(), textUpdate(testTrustedChat+1, "/halt"))

	if called, _ := cmd.didHalt(); called {
		t.Fatalf("Halt must NOT be called from an untrusted chat")
	}
	if send.count() != 0 {
		t.Fatalf("an untrusted chat must get NO reply, got %d: %+v", send.count(), send.snapshot())
	}
}

// 3. /status reply contains the mode + halted state from a fake Status().
func TestStatusReplyReflectsStatusInfo(t *testing.T) {
	r, cmd, send := newRouterFixture()
	cmd.status = StatusInfo{
		Halted:     true,
		HaltReason: "daily loss limit hit",
		Mode:       "paper",
		Broker:     "alpaca",
		Equity:     "10000.00",
	}

	r.Handle(context.Background(), textUpdate(testTrustedChat, "/status"))

	got := send.lastText()
	if !strings.Contains(got, "paper") {
		t.Fatalf("status reply must mention the mode, got %q", got)
	}
	if !strings.Contains(strings.ToUpper(got), "HALTED") {
		t.Fatalf("status reply must mention halted state, got %q", got)
	}
	if !strings.Contains(got, "daily loss limit hit") {
		t.Fatalf("status reply must mention the halt reason, got %q", got)
	}
	if !strings.Contains(got, "alpaca") {
		t.Fatalf("status reply must mention the broker, got %q", got)
	}
}

func TestStatusReplyActiveWhenNotHalted(t *testing.T) {
	r, cmd, send := newRouterFixture()
	cmd.status = StatusInfo{Halted: false, Mode: "live", Broker: "alpaca", Equity: "500.00"}

	r.Handle(context.Background(), textUpdate(testTrustedChat, "/status"))

	got := strings.ToLower(send.lastText())
	if !strings.Contains(got, "active") {
		t.Fatalf("status reply must say trading active when not halted, got %q", send.lastText())
	}
}

// 4. /summary reply contains equity + realized/unrealized from a fake Report().
func TestSummaryReplyReflectsReport(t *testing.T) {
	r, cmd, send := newRouterFixture()
	cmd.report = channel.Report{
		Equity:       orders.MustParseDecimal("12345.67"),
		RealizedPL:   orders.MustParseDecimal("100.50"),
		UnrealizedPL: orders.MustParseDecimal("-25.25"),
		Positions: []channel.Position{
			{Symbol: "AAPL"},
			{Symbol: "TSLA"},
		},
	}

	r.Handle(context.Background(), textUpdate(testTrustedChat, "/summary"))

	got := send.lastText()
	for _, want := range []string{"12345.67", "100.50", "-25.25"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary reply must contain %q, got %q", want, got)
		}
	}
	if !strings.Contains(got, "2") {
		t.Fatalf("summary reply must mention 2 open positions, got %q", got)
	}
}

// 5. /positions: with positions → reply lists them; empty → "no open positions".
func TestPositionsReplyListsPositions(t *testing.T) {
	r, cmd, send := newRouterFixture()
	cmd.report = channel.Report{
		Positions: []channel.Position{
			{
				Symbol:       "AAPL",
				Qty:          orders.MustParseDecimal("10"),
				MarkPx:       orders.MustParseDecimal("150.25"),
				UnrealizedPL: orders.MustParseDecimal("52.50"),
			},
			{
				Symbol:       "TSLA",
				Qty:          orders.MustParseDecimal("-3"),
				MarkPx:       orders.MustParseDecimal("700.00"),
				UnrealizedPL: orders.MustParseDecimal("-12.00"),
			},
		},
	}

	r.Handle(context.Background(), textUpdate(testTrustedChat, "/positions"))

	got := send.lastText()
	for _, want := range []string{"AAPL", "150.25", "52.50", "TSLA", "-3", "700.00", "-12.00"} {
		if !strings.Contains(got, want) {
			t.Fatalf("positions reply must contain %q, got %q", want, got)
		}
	}
}

func TestPositionsReplyEmpty(t *testing.T) {
	r, cmd, send := newRouterFixture()
	cmd.report = channel.Report{} // no positions

	r.Handle(context.Background(), textUpdate(testTrustedChat, "/positions"))

	if !strings.Contains(strings.ToLower(send.lastText()), "no open positions") {
		t.Fatalf("empty positions must reply 'no open positions', got %q", send.lastText())
	}
}

// 6. /help and an unknown command both reply with the command list. A non-command
// message (plain text) from the trusted chat replies help (chosen behavior).
func TestHelpReplyListsCommands(t *testing.T) {
	r, _, send := newRouterFixture()

	r.Handle(context.Background(), textUpdate(testTrustedChat, "/help"))

	got := send.lastText()
	for _, want := range []string{"/status", "/summary", "/positions", "/halt", "/resume"} {
		if !strings.Contains(got, want) {
			t.Fatalf("/help reply must list %q, got %q", want, got)
		}
	}
}

func TestUnknownCommandRepliesHelp(t *testing.T) {
	r, _, send := newRouterFixture()

	r.Handle(context.Background(), textUpdate(testTrustedChat, "/wat"))

	got := send.lastText()
	if !strings.Contains(got, "/status") || !strings.Contains(got, "/halt") {
		t.Fatalf("unknown command must reply with the command list, got %q", got)
	}
}

func TestPlainTextRepliesHelp(t *testing.T) {
	r, cmd, send := newRouterFixture()

	r.Handle(context.Background(), textUpdate(testTrustedChat, "hello there"))

	// No action taken.
	if called, _ := cmd.didHalt(); called {
		t.Fatalf("plain text must not trigger Halt")
	}
	got := send.lastText()
	if !strings.Contains(got, "/status") {
		t.Fatalf("plain text from trusted chat must reply with help, got %q", got)
	}
}

// 7. A callback_query update (Message nil) is IGNORED by this router — callbacks
// belong to the approval handler, not the command router.
func TestCallbackQueryIsIgnored(t *testing.T) {
	r, cmd, send := newRouterFixture()

	cb := &CallbackQuery{ID: "cb1", Data: "approve:abc"}
	msg := &Message{}
	msg.Chat.ID = testTrustedChat
	cb.Message = msg
	u := Update{UpdateID: 9, CallbackQuery: cb} // Message is nil

	r.Handle(context.Background(), u)

	if called, _ := cmd.didHalt(); called {
		t.Fatalf("a callback_query must not trigger any command action")
	}
	if send.count() != 0 {
		t.Fatalf("a callback_query must not be consumed by the command router (no Send), got %d", send.count())
	}
}

func TestEmptyTextIsIgnored(t *testing.T) {
	r, _, send := newRouterFixture()

	r.Handle(context.Background(), textUpdate(testTrustedChat, ""))

	if send.count() != 0 {
		t.Fatalf("an empty-text message must be ignored, got %d sends", send.count())
	}
}

// 8. /status@BucksBot (bot-suffixed) is treated as /status.
func TestBotSuffixedCommandIsParsed(t *testing.T) {
	r, cmd, send := newRouterFixture()
	cmd.status = StatusInfo{Mode: "paper", Broker: "alpaca", Equity: "1.00"}

	r.Handle(context.Background(), textUpdate(testTrustedChat, "/status@BucksBot"))

	if send.count() != 1 {
		t.Fatalf("expected /status@BucksBot to be handled as /status, got %d sends", send.count())
	}
	if !strings.Contains(send.lastText(), "paper") {
		t.Fatalf("bot-suffixed /status must produce the status reply, got %q", send.lastText())
	}
}

func TestHaltErrorRepliesPlainError(t *testing.T) {
	r, cmd, send := newRouterFixture()
	cmd.haltErr = context.DeadlineExceeded

	r.Handle(context.Background(), textUpdate(testTrustedChat, "/halt"))

	if send.count() != 1 {
		t.Fatalf("expected one error reply, got %d", send.count())
	}
	if strings.Contains(send.lastText(), "HALTED") {
		t.Fatalf("on Halt error we must NOT claim halted, got %q", send.lastText())
	}
}
