package gateway

import (
	"context"
	"strings"
	"testing"
)

// TestCommandRouterTOFUPairing proves opt-in trust-on-first-use pairing: with no trusted chat,
// the FIRST message pairs (the persist callback fires) and confirms WITHOUT executing its
// command; the paired chat can then command; and any other chat is ignored.
func TestCommandRouterTOFUPairing(t *testing.T) {
	cmd := &fakeCommandContext{}
	send := &fakeSender{}
	var paired int64
	r := NewCommandRouter(0, cmd, send, WithPairing(func(id int64) bool { paired = id; return true }))

	// 1) First message pairs (callback persisted it), confirms, and does NOT execute /halt.
	r.Handle(context.Background(), textUpdate(5550, "/halt"))
	if paired != 5550 {
		t.Fatalf("first message must pair chat 5550, got %d", paired)
	}
	if called, _ := cmd.didHalt(); called {
		t.Error("the pairing message must NOT execute its command (/halt)")
	}
	if send.count() != 1 || !strings.Contains(send.lastText(), "Paired") {
		t.Errorf("expected one 'Paired' confirmation, got %d sends, last=%q", send.count(), send.lastText())
	}

	// 2) The now-paired chat can command.
	r.Handle(context.Background(), textUpdate(5550, "/halt"))
	if called, _ := cmd.didHalt(); !called {
		t.Error("the paired chat's /halt should halt")
	}

	// 3) A DIFFERENT chat is ignored after pairing — only the paired chat is trusted.
	sendsBefore := send.count()
	r.Handle(context.Background(), textUpdate(9999, "/status"))
	if send.count() != sendsBefore {
		t.Error("a non-paired chat must be ignored after pairing (no reply)")
	}
}

// TestCommandRouterPairingRejectedFailsClosed proves that if the persist callback rejects the
// pairing (e.g. the trust file couldn't be written), the router does NOT trust the chat and
// stays silent — fail closed, never fail open.
func TestCommandRouterPairingRejectedFailsClosed(t *testing.T) {
	cmd := &fakeCommandContext{}
	send := &fakeSender{}
	r := NewCommandRouter(0, cmd, send, WithPairing(func(int64) bool { return false }))

	r.Handle(context.Background(), textUpdate(5550, "/halt"))

	if called, _ := cmd.didHalt(); called {
		t.Error("a rejected pairing must NOT trust the chat or execute its command")
	}
	if send.count() != 0 {
		t.Error("a rejected pairing must stay silent (fail-closed)")
	}
}
