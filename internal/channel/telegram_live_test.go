//go:build telegram_live

package channel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bucks/internal/orders"
)

// These tests compile and run ONLY under `go test -tags telegram_live`. They never
// touch api.telegram.org: a local httptest server stands in for the Bot API, so
// even the live path is exercised against a fake — no real operator is paged. They
// exist to prove the TelegramChannel satisfies the interface and its
// construction/fail-safe contract hold; they are NOT part of the default suite.

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

// newFakeTelegram builds a TelegramChannel pointed at a local httptest server (NOT
// the real Bot API) so the live HTTP path is exercised offline.
func newFakeTelegram(t *testing.T, handler http.HandlerFunc) (*TelegramChannel, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Setenv(envTelegramToken, "fake-token")
	t.Setenv(envTelegramChatID, "12345")
	ch, err := NewTelegramChannel(WithHTTPClient(srv.Client()), WithPollInterval(time.Millisecond))
	if err != nil {
		srv.Close()
		t.Fatalf("construct: %v", err)
	}
	ch.apiBase = srv.URL // redirect to the fake server
	return ch, srv
}

// TestTelegramChannel_SendAlert proves the live SendAlert posts to sendMessage.
func TestTelegramChannel_SendAlert(t *testing.T) {
	var hit string
	ch, srv := newFakeTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		hit = r.URL.Path
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	})
	defer srv.Close()
	if err := ch.SendAlert(context.Background(), Alert{Level: AlertInfo, Text: "live-hb"}); err != nil {
		t.Fatalf("send alert: %v", err)
	}
	if !strings.HasSuffix(hit, "/sendMessage") {
		t.Fatalf("expected sendMessage, hit %q", hit)
	}
}

// TestTelegramChannel_ApprovalTimeoutDenies proves the live approval path fails
// SAFE: when no operator tap arrives before the deadline, it returns Denied.
func TestTelegramChannel_ApprovalTimeoutDenies(t *testing.T) {
	ch, srv := newFakeTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		// sendMessage ok; getUpdates always empty (operator never taps).
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	})
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	d, err := ch.RequestApproval(ctx, ApprovalRequest{Symbol: "AAPL", Qty: liveDec(t, "1")})
	if d.Approved() {
		t.Fatalf("no tap before deadline must be Denied, got %s", d)
	}
	if err == nil {
		t.Fatalf("expected the deadline error")
	}
}

// TestTelegramChannel_ApprovalApproved proves a real Approve tap (via the fake
// getUpdates) returns Approved.
func TestTelegramChannel_ApprovalApproved(t *testing.T) {
	ch, srv := newFakeTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":7,"callback_query":{"data":"bucks_approve"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	})
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, err := ch.RequestApproval(ctx, ApprovalRequest{Symbol: "AAPL", Qty: liveDec(t, "1")})
	if err != nil || !d.Approved() {
		t.Fatalf("approve tap: got (%s,%v), want (Approved,nil)", d, err)
	}
}

// compile-time interface check under the tag.
var _ Channel = (*TelegramChannel)(nil)
