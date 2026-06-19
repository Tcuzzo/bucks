package channel

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// netDialAttempts counts every real network-dial attempt made by ANY test in this
// package's default suite. It is wired into the process's actual outbound-network
// path by TestMain (below), which replaces http.DefaultTransport with a transport
// whose dialer increments this counter and then FAILS the dial. So a dial is both
// COUNTED and BLOCKED: nothing in the default suite can reach a live operator, and
// any accidental attempt is recorded for the assertions to catch.
//
// This is a REAL guard, not a self-incrementing stub: the counter only moves if
// some code path genuinely tries to open a socket through the default transport
// (which is what the live Telegram channel uses unless given a custom client). The
// in-memory MockChannel has no transport at all, so the counter stays zero through a
// full exercise of every Channel method — that is the proof a normal `go test ./...`
// cannot page a real operator.
var netDialAttempts int64

// errNoLiveDial is returned for any dial attempted from the default test suite. It
// blocks the connection (fail-safe) in addition to counting it.
var errNoLiveDial = errors.New("channel: live network dial blocked in default test suite")

// TestMain installs a counting+blocking transport as http.DefaultTransport for the
// whole default suite, runs the tests, restores the original transport, and asserts
// that ZERO real network dials were attempted. If any default-suite test path opened
// (or tried to open) a socket through the default transport, the counter would be
// non-zero and the suite fails — catching a leaked live channel call at the seam.
func TestMain(m *testing.M) {
	orig := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		// DialContext counts the attempt and refuses it — no socket is ever opened.
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			atomic.AddInt64(&netDialAttempts, 1)
			return nil, errNoLiveDial
		},
		// Conservative timeouts in case a path bypasses DialContext somehow; the
		// dialer above is the authoritative block.
		TLSHandshakeTimeout:   time.Second,
		ResponseHeaderTimeout: time.Second,
	}

	code := m.Run()

	http.DefaultTransport = orig
	if n := atomic.LoadInt64(&netDialAttempts); n != 0 {
		// Surface the leak loudly and fail the whole package run.
		panic("channel default suite attempted " + strconv.FormatInt(n, 10) + " real network dial(s) — a live channel call leaked")
	}
	os.Exit(code)
}

// TestNoLiveTelegramInDefaultSuite drives every Channel method on the in-memory
// MockChannel and asserts the shared dial counter (wired into http.DefaultTransport
// by TestMain) did not move. Because the counter ONLY increments on a genuine
// outbound dial, this is now a real assertion: a live channel call would open a
// socket through the default transport, increment the counter, and fail the test.
// The live channel is compiled into the default build (no build tag), so this
// behavioral dial-blocker — not a structural fence — is the full "no live call in
// the default suite" guarantee; the hermetic live tests reach a local httptest
// server through their own transport, never http.DefaultTransport.
func TestNoLiveTelegramInDefaultSuite(t *testing.T) {
	before := atomic.LoadInt64(&netDialAttempts)

	m := NewMockChannel()
	m.ScriptApprove()

	if err := m.SendAlert(context.Background(), Alert{Text: "no-net"}); err != nil {
		t.Fatalf("send alert: %v", err)
	}
	d, err := m.RequestApproval(context.Background(), ApprovalRequest{Symbol: "AAPL"})
	if err != nil || !d.Approved() {
		t.Fatalf("approval: got (%s,%v)", d, err)
	}
	if err := m.SendReport(context.Background(), Report{}); err != nil {
		t.Fatalf("send report: %v", err)
	}

	if got := atomic.LoadInt64(&netDialAttempts); got != before {
		t.Fatalf("MockChannel exercise made %d network dial(s) — a live channel call leaked",
			got-before)
	}
}
