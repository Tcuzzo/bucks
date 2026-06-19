package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bucks/internal/channel"
	"bucks/internal/gateway"
	"bucks/internal/risk"
	"bucks/internal/secrets"
)

// mockTelegram is a hermetic stand-in for the Telegram Bot API on the REAL --daemon entry
// point. It serves getUpdates from a scripted queue of updates (a /status command, then a
// /halt command, then empty forever) and records every sendMessage the daemon posts back,
// so the test can assert the assembled gateway actually serves commands and trips the
// durable kill switch. No real network — the daemon's HTTP client is pointed here.
type mockTelegram struct {
	mu sync.Mutex
	// queued updates to hand out, in order; each getUpdates pops the ones past the
	// requested offset. After they are drained it returns an empty result (long-poll idle).
	updates []map[string]any
	// sendMessage posts the daemon made (chat_id + text), so the test can assert the reply.
	sent []sentMessage
	// signals so the test can wait for the daemon to have served each step without sleeping.
	statusReplied chan struct{}
}

type sentMessage struct {
	chatID float64
	text   string
}

func newMockTelegram(trustedChatID int64) *mockTelegram {
	return &mockTelegram{
		updates: []map[string]any{
			// update 1: the operator sends /status from the trusted chat.
			textUpdate(1, trustedChatID, "/status"),
			// update 2: the operator sends /halt from the trusted chat.
			textUpdate(2, trustedChatID, "/halt"),
		},
		statusReplied: make(chan struct{}, 1),
	}
}

// textUpdate builds a Telegram getUpdates "message" update carrying text from chatID.
func textUpdate(updateID, chatID int64, text string) map[string]any {
	return map[string]any{
		"update_id": updateID,
		"message": map[string]any{
			"message_id": updateID,
			"chat":       map[string]any{"id": chatID},
			"text":       text,
		},
	}
}

func (m *mockTelegram) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			m.serveGetUpdates(w, r)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			m.serveSendMessage(w, r)
		default:
			// Any other method (the daemon does not call one in this test) just OKs.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		}
	})
}

// serveGetUpdates returns the queued updates whose id is >= the requested offset, then
// drains them so a subsequent poll returns empty (the idle long-poll). offset is the next
// id the gateway wants (Load()+1, advanced past each handled update).
func (m *mockTelegram) serveGetUpdates(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	var out []map[string]any
	out = append(out, m.updates...)
	m.updates = nil // hand each update out exactly once
	m.mu.Unlock()

	// When the queue is drained, HOLD the connection like a real long-poll instead of
	// busy-returning empty (which would spin the gateway). Return empty when the request
	// context ends (the daemon's shutdown cancels it, or the long-poll deadline fires).
	if len(out) == 0 {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"ok": true, "result": out}
	_ = json.NewEncoder(w).Encode(resp)
}

func (m *mockTelegram) serveSendMessage(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)

	cid, _ := body["chat_id"].(float64)
	text, _ := body["text"].(string)
	m.mu.Lock()
	m.sent = append(m.sent, sentMessage{chatID: cid, text: text})
	m.mu.Unlock()

	// A /status reply carries the BUCKS status banner; signal the test it landed.
	if strings.Contains(text, "BUCKS status") {
		select {
		case m.statusReplied <- struct{}{}:
		default:
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
}

func (m *mockTelegram) sentMessages() []sentMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sentMessage, len(m.sent))
	copy(out, m.sent)
	return out
}

// TestAssembleDaemonWiresChannelApproverToRegistry proves the SINGLE-POLLER wiring: the
// operator channel's Approver is the gateway's approval registry, so an above-band approval
// the channel raises is resolved by a tap routed through the ONE gateway dispatch — never a
// second getUpdates poller. We assemble the daemon, raise an approval through the channel,
// then deliver the matching Approve tap via the gateway's Mux callback path and assert the
// channel's RequestApproval returns Approved.
func TestAssembleDaemonWiresChannelApproverToRegistry(t *testing.T) {
	const trustedChatID = int64(424242)

	rec := &keyboardRecorder{tokens: make(chan string, 1)}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	t.Setenv("BUCKS_TELEGRAM_BOT_TOKEN", "test-token")
	t.Setenv("BUCKS_TELEGRAM_CHAT_ID", "424242")

	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	r := validSetupResult(t)
	cfg := daemonConfig{
		apiBase:    srv.URL,
		httpClient: srv.Client(),
		logw:       io.Discard,
	}
	logf := func(string, ...any) {}

	asm, err := assembleDaemon(configPath, "test-token", trustedChatID, r, cfg, logf)
	if err != nil {
		t.Fatalf("assembleDaemon: %v", err)
	}
	if asm.channel == nil {
		t.Fatal("operator channel was not assembled — single-poller wiring not exercised")
	}

	// Raise an approval through the CHANNEL (what the trade loop does). It blocks until the
	// routed tap resolves it. We feed the matching Approve tap through the gateway registry
	// (the Mux's callback handler) in a goroutine.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	decisionCh := make(chan struct {
		d   bool
		err error
	}, 1)
	go func() {
		dec, derr := asm.channel.RequestApproval(ctx, channelApprovalRequest())
		decisionCh <- struct {
			d   bool
			err error
		}{dec.Approved(), derr}
	}()

	// Wait until the registry has posted the keyboard (so the token exists), then read the
	// token it carried and deliver an Approve callback through the SAME registry the gateway
	// dispatch would route to.
	tok := rec.awaitToken(t, 3*time.Second)
	asm.registry.Handle(context.Background(), gateway.Update{
		UpdateID:      1,
		CallbackQuery: &gateway.CallbackQuery{ID: "cb", Data: "bucks:approve:" + tok},
	})

	select {
	case got := <-decisionCh:
		if got.err != nil {
			t.Errorf("RequestApproval returned err: %v", got.err)
		}
		if !got.d {
			t.Error("routed Approve tap did not resolve the channel approval as Approved")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("channel approval never resolved — the channel.Approver is not the gateway registry")
	}
}

// channelApprovalRequest is a minimal above-band approval request for the wiring test.
func channelApprovalRequest() channel.ApprovalRequest {
	return channel.ApprovalRequest{Summary: "Approve this above-band trade?"}
}

// TestBuildDaemonChannelUsesWizardTokenNoEnv is the FINDING-2 regression test: a wizard-only
// operator — the token saved in encrypted secrets, with BUCKS_TELEGRAM_BOT_TOKEN explicitly
// UNSET — must still get a routine channel. buildDaemonChannel must construct it from the
// explicit wizard-saved token + trusted chat id (the SAME ones the gateway uses), never
// depending on the env var. Before the unify fix this FAILS (the channel read the env var
// and a wizard-only operator got a gateway but no channel). Hermetic — no network.
func TestBuildDaemonChannelUsesWizardTokenNoEnv(t *testing.T) {
	// The wizard-only contract: the bot-token env var is NOT set. (The chat id env is the
	// gateway's trust gate and is read separately; here we pass the chat id explicitly too.)
	t.Setenv("BUCKS_TELEGRAM_BOT_TOKEN", "")
	t.Setenv("BUCKS_TELEGRAM_CHAT_ID", "")

	const wizardToken = "wizard-saved-secret-token"
	const trustedChatID = int64(900900)

	ch, err := buildDaemonChannel(nil, wizardToken, trustedChatID)
	if err != nil {
		t.Fatalf("wizard-only setup (explicit token, no env) must yield a channel: %v", err)
	}
	if ch == nil {
		t.Fatal("buildDaemonChannel returned a nil channel for a valid wizard token")
	}

	// And with NEITHER an explicit token NOR the env var, it must still fail SAFE (no channel),
	// proving we did not weaken the "needs a token" guarantee.
	if _, err := buildDaemonChannel(nil, "", 0); err == nil {
		t.Fatal("buildDaemonChannel with no token and no env must return an error (fail-safe)")
	}
}

// keyboardRecorder is an httptest handler that records the approval-keyboard sendMessage
// the registry posts and hands the test the token it carried (so the test can deliver the
// matching tap). No network leaves the test.
type keyboardRecorder struct {
	tokens chan string
}

func (k *keyboardRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if tok := tokenFromKeyboardBody(body); tok != "" {
			select {
			case k.tokens <- tok:
			default:
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	})
}

func (k *keyboardRecorder) awaitToken(t *testing.T, d time.Duration) string {
	t.Helper()
	select {
	case tok := <-k.tokens:
		return tok
	case <-time.After(d):
		t.Fatal("registry never posted an approval keyboard")
		return ""
	}
}

// tokenFromKeyboardBody pulls the token out of the first "bucks:approve:<token>"
// callback_data in a posted inline keyboard, so the test can route the matching tap.
func tokenFromKeyboardBody(body map[string]any) string {
	rm, ok := body["reply_markup"].(map[string]any)
	if !ok {
		return ""
	}
	rows, ok := rm["inline_keyboard"].([]any)
	if !ok {
		return ""
	}
	for _, row := range rows {
		btns, ok := row.([]any)
		if !ok {
			continue
		}
		for _, b := range btns {
			btn, ok := b.(map[string]any)
			if !ok {
				continue
			}
			cd, ok := btn["callback_data"].(string)
			if !ok {
				continue
			}
			const prefix = "bucks:approve:"
			if strings.HasPrefix(cd, prefix) {
				return strings.TrimPrefix(cd, prefix)
			}
		}
	}
	return ""
}

// TestRunDaemonServesStatusAndHaltsOnRealEntryPoint is the capstone e2e: with a persisted
// config and a hermetic mock Telegram, the REAL runDaemon assembles the gateway, serves a
// /status command (posting the status text back), trips the durable kill switch on /halt,
// and shuts down promptly when the context is canceled. Unsteered — it drives the actual
// assembled daemon, not injected internals.
func TestRunDaemonServesStatusAndHaltsOnRealEntryPoint(t *testing.T) {
	const trustedChatID = int64(777001)

	// Persist a real config (the same persistSetup the wizard uses), hermetic file backend.
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "daemon-e2e-pass"
	if err := persistSetup(validSetupResult(t), configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("persistSetup: %v", err)
	}
	// The daemon reads the passphrase + chat id from the environment (the headless box's
	// contract). t.Setenv restores them after the test.
	t.Setenv("BUCKS_PASSPHRASE", pass)
	t.Setenv("BUCKS_TELEGRAM_CHAT_ID", "777001")
	// The live channel reads its own token+chat from env; set both so buildDaemonChannel
	// succeeds and the approver wiring path is exercised (it points at the mock via client).
	t.Setenv("BUCKS_TELEGRAM_BOT_TOKEN", "test-token")

	mock := newMockTelegram(trustedChatID)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, configPath,
			withApiBase(srv.URL),
			withHTTPClient(srv.Client()),
			withLogWriter(io.Discard),
			withSecretOpts(secrets.ForceFileBackend()),
		)
	}()

	// 1. The daemon must POST a /status reply back (it served the /status command).
	select {
	case <-mock.statusReplied:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not post a /status reply within 5s")
	}

	// 2. The /halt command must trip the DURABLE kill switch on disk. Poll the durable file
	//    directly (a fresh Open reads what the daemon persisted) until it reports halted.
	ksPath := filepath.Join(dir, "killswitch.json")
	deadline := time.After(5 * time.Second)
	halted := false
	for !halted {
		select {
		case <-deadline:
			t.Fatal("daemon did not trip the durable kill switch on /halt within 5s")
		default:
		}
		ks, err := risk.Open(ksPath)
		if err == nil {
			if h, _ := ks.IsHalted(); h {
				halted = true
				if ks.Kind() != risk.HaltManual {
					t.Errorf("halt kind = %s, want Manual (operator /halt)", ks.Kind())
				}
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 3. The status reply text actually carries the loaded state (mode + equity), proving
	//    the CommandContext is config-backed, not a stub.
	var statusText string
	for _, s := range mock.sentMessages() {
		if strings.Contains(s.text, "BUCKS status") {
			statusText = s.text
			if int64(s.chatID) != trustedChatID {
				t.Errorf("status reply chat_id = %v, want %d", s.chatID, trustedChatID)
			}
		}
	}
	if statusText == "" {
		t.Fatal("no BUCKS status reply was recorded")
	}
	if !strings.Contains(statusText, "Mode: paper") {
		t.Errorf("status text missing paper mode:\n%s", statusText)
	}
	if !strings.Contains(statusText, "25000") {
		t.Errorf("status text missing the loaded equity (25000):\n%s", statusText)
	}

	// 4. Graceful shutdown: cancel the ctx and runDaemon must return promptly.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runDaemon returned a non-nil error on cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon did not return promptly after ctx cancel (graceful shutdown failed)")
	}
}
