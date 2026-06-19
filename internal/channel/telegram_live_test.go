package channel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"bucks/internal/orders"
)

// These tests run in the DEFAULT suite (the live channel is no longer build-tagged,
// so it ships in the binary). They never touch api.telegram.org: a local httptest
// server stands in for the Bot API, reached through the test's own transport (never
// http.DefaultTransport, which the dial-blocker in nolive_test.go refuses) — so no
// real operator is ever paged. They prove the TelegramChannel satisfies the interface
// and its construction / fail-safe / no-self-poll contracts hold.

func liveDec(t *testing.T, s string) Decimal {
	t.Helper()
	d, err := orders.ParseDecimal(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return d
}

// TestNewTelegramChannel_RequiresToken proves construction fails loudly without the
// trader's OWN bot token (never tokenless, never a shared token).
func TestNewTelegramChannel_RequiresToken(t *testing.T) {
	t.Setenv(envTelegramToken, "")
	if _, err := NewTelegramChannel(); err == nil {
		t.Fatalf("expected error when %s is unset", envTelegramToken)
	}
}

// TestNewTelegramChannel_ExplicitTokenNoEnv is the FINDING-2 regression test: a wizard-only
// operator (the token saved in encrypted secrets, NOT exported as BUCKS_TELEGRAM_BOT_TOKEN)
// must still get a working channel. With the env var explicitly UNSET, an explicit token +
// chat id passed via WithToken/WithChatID must construct a channel whose apiBase carries that
// token and whose chatID matches — proving the daemon can feed the wizard-saved secret in
// without divergence. Before the seam exists this FAILS (construction errors with no env).
func TestNewTelegramChannel_ExplicitTokenNoEnv(t *testing.T) {
	// Prove no reliance on the env: both vars are explicitly empty for this test.
	t.Setenv(envTelegramToken, "")
	t.Setenv(envTelegramChatID, "")

	const tok = "wizard-saved-token-9988"
	const chat = int64(555111)

	ch, err := NewTelegramChannel(WithToken(tok), WithChatID(chat))
	if err != nil {
		t.Fatalf("wizard-only construction (explicit token, no env) must succeed: %v", err)
	}
	if !strings.Contains(ch.apiBase, tok) {
		t.Fatalf("apiBase must carry the explicit token; got %q", ch.apiBase)
	}
	if ch.chatID != strconv.FormatInt(chat, 10) {
		t.Fatalf("chatID = %q, want %q", ch.chatID, strconv.FormatInt(chat, 10))
	}
}

// TestNewTelegramChannel_ExplicitTokenOverridesEnv proves an explicit token wins over the
// env var (the daemon's wizard-saved secret is authoritative), while leaving the env-only
// path intact for existing callers (covered by the other tests, which set the env).
func TestNewTelegramChannel_ExplicitTokenOverridesEnv(t *testing.T) {
	t.Setenv(envTelegramToken, "env-token-should-lose")
	t.Setenv(envTelegramChatID, "999")

	ch, err := NewTelegramChannel(WithToken("explicit-wins"), WithChatID(int64(42)))
	if err != nil {
		t.Fatalf("construction: %v", err)
	}
	if strings.Contains(ch.apiBase, "env-token-should-lose") {
		t.Fatalf("explicit token must override the env token; apiBase=%q", ch.apiBase)
	}
	if !strings.Contains(ch.apiBase, "explicit-wins") {
		t.Fatalf("apiBase must carry the explicit token; got %q", ch.apiBase)
	}
	if ch.chatID != "42" {
		t.Fatalf("explicit chat id must win; chatID=%q", ch.chatID)
	}
}

// recordingTransport records every Telegram method path the channel hits, so a test
// can assert getUpdates is NEVER called (the single-poller invariant: approvals are
// routed through the gateway, not self-polled here).
type recordingTransport struct {
	mu      sync.Mutex
	methods []string
	inner   http.RoundTripper
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.methods = append(rt.methods, req.URL.Path)
	rt.mu.Unlock()
	return rt.inner.RoundTrip(req)
}

func (rt *recordingTransport) called(suffix string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, m := range rt.methods {
		if strings.HasSuffix(m, suffix) {
			return true
		}
	}
	return false
}

// newFakeTelegram builds a TelegramChannel pointed at a local httptest server (NOT
// the real Bot API) so the live HTTP path is exercised offline. It returns a
// recordingTransport so a test can assert which Bot API methods were hit.
func newFakeTelegram(t *testing.T, handler http.HandlerFunc) (*TelegramChannel, *httptest.Server, *recordingTransport) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Setenv(envTelegramToken, "fake-token")
	t.Setenv(envTelegramChatID, "12345")
	rt := &recordingTransport{inner: srv.Client().Transport}
	client := &http.Client{Transport: rt}
	ch, err := NewTelegramChannel(WithHTTPClient(client))
	if err != nil {
		srv.Close()
		t.Fatalf("construct: %v", err)
	}
	ch.apiBase = srv.URL // redirect to the fake server
	return ch, srv, rt
}

// TestTelegramChannel_SendAlert proves the live SendAlert posts to sendMessage.
func TestTelegramChannel_SendAlert(t *testing.T) {
	ch, srv, rt := newFakeTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	})
	defer srv.Close()
	if err := ch.SendAlert(context.Background(), Alert{Level: AlertInfo, Text: "live-hb"}); err != nil {
		t.Fatalf("send alert: %v", err)
	}
	if !rt.called("/sendMessage") {
		t.Fatalf("expected sendMessage to be hit")
	}
}

// fakeApprover is a scripted channel.Approver: it returns a canned decision and
// records that it was asked. It is the gateway's ApprovalRegistry in production; here
// it stands in so the cutover can be proven WITHOUT a second poller.
type fakeApprover struct {
	decision Decision
	err      error
	called   bool
}

func (f *fakeApprover) RequestApproval(_ context.Context, _ ApprovalRequest) (Decision, error) {
	f.called = true
	return f.decision, f.err
}

// 7a. With an injected Approver, RequestApproval returns whatever the Approver decides
// and NEVER issues getUpdates (no second poller — the whole point of the cutover).
func TestTelegramChannel_DelegatesToApproverNoSelfPoll(t *testing.T) {
	ch, srv, rt := newFakeTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		// If the channel ever self-polls, this records a /getUpdates hit which the
		// assertion below catches. We answer ok regardless so a stray call wouldn't
		// hang the test — it would simply be detected.
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	})
	defer srv.Close()

	app := &fakeApprover{decision: DecisionApproved}
	ch.SetApprover(app)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, err := ch.RequestApproval(ctx, ApprovalRequest{Symbol: "AAPL", Qty: liveDec(t, "1")})
	if err != nil {
		t.Fatalf("delegated approval returned error: %v", err)
	}
	if !d.Approved() {
		t.Fatalf("must return the Approver's decision (Approved), got %s", d)
	}
	if !app.called {
		t.Fatalf("RequestApproval must delegate to the injected Approver")
	}
	if rt.called("/getUpdates") {
		t.Fatalf("RequestApproval issued getUpdates — there must be NO second poller; approvals route through the gateway")
	}
}

// 7b. The fail-safe is preserved through delegation: an Approver that denies (e.g. on
// timeout/transport error in production) yields Denied.
func TestTelegramChannel_DelegatedDenyStaysDenied(t *testing.T) {
	ch, srv, rt := newFakeTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	})
	defer srv.Close()

	app := &fakeApprover{decision: DecisionDenied, err: context.DeadlineExceeded}
	ch.SetApprover(app)

	d, err := ch.RequestApproval(context.Background(), ApprovalRequest{Symbol: "AAPL", Qty: liveDec(t, "1")})
	if d.Approved() {
		t.Fatalf("a denying Approver must yield Denied, got %s", d)
	}
	if err == nil {
		t.Fatalf("the Approver's error must surface for logging")
	}
	if rt.called("/getUpdates") {
		t.Fatalf("no self-poll on the deny path either")
	}
}

// compile-time interface check under the tag.
var _ Channel = (*TelegramChannel)(nil)
