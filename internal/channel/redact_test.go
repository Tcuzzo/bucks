package channel

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// roundTripFunc adapts a function to http.RoundTripper so a test can force a transport error.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestTelegramChannelErrorsRedactBotToken proves a transport failure on a send NEVER leaks the
// bot token. http.Client.Do wraps a RoundTripper error in a *url.Error whose message is
// `Post "https://api.telegram.org/bot<TOKEN>/sendMessage": ...` — embedding the full token.
// The trade loop returns and logs that error on routine transient faults, so an un-redacted
// wrap would write the token (full control of the operator's bot) into the logs.
func TestTelegramChannelErrorsRedactBotToken(t *testing.T) {
	const token = "123456789:SUPER-SECRET-BOT-TOKEN-xyz"
	failClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}
	ch, err := NewTelegramChannel(WithToken(token), WithChatID(123), WithHTTPClient(failClient))
	if err != nil {
		t.Fatalf("NewTelegramChannel: %v", err)
	}
	serr := ch.SendAlert(context.Background(), Alert{Level: AlertInfo, Text: "heartbeat"})
	if serr == nil {
		t.Fatal("expected a transport error from the failing client")
	}
	if strings.Contains(serr.Error(), token) || strings.Contains(serr.Error(), "SUPER-SECRET") {
		t.Errorf("bot token LEAKED into the returned error: %q", serr.Error())
	}
	if !strings.Contains(serr.Error(), "redacted") {
		t.Errorf("expected the token-bearing URL to be redacted in the error, got %q", serr.Error())
	}
}

// TestRedactedErrorPreservesChain proves redaction does NOT break error identity — a wrapped
// context.Canceled is still detectable via errors.Is, so shutdown classification is intact.
func TestRedactedErrorPreservesChain(t *testing.T) {
	wrapped := &redactedError{err: context.Canceled, apiBase: "https://api.telegram.org/bot123:SECRET"}
	if !errors.Is(wrapped, context.Canceled) {
		t.Error("redactedError must Unwrap to preserve errors.Is")
	}
	if strings.Contains(wrapped.Error(), "SECRET") {
		t.Errorf("redactedError.Error() leaked the token: %q", wrapped.Error())
	}
}
