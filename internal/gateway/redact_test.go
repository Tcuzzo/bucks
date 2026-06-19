package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// The fake token used across the redaction tests. It is shaped like a real Telegram
// bot token (<bot-id>:<secret>) so the tests prove the WHOLE secret never leaks.
const fakeToken = "123456789:AAFakeSecretToken"

const fakeAPIBase = "https://api.telegram.org/bot" + fakeToken

// TestRedactBase proves redactBase scrubs every occurrence of the token-bearing apiBase
// out of an arbitrary string (e.g. a *url.Error message), leaving a <redacted> marker and
// no trace of the secret. An empty apiBase is a no-op (nothing to redact).
func TestRedactBase(t *testing.T) {
	in := "Get \"" + fakeAPIBase + "/getUpdates?offset=1\": dial tcp: connection refused"
	out := redactBase(in, fakeAPIBase)
	if strings.Contains(out, fakeToken) {
		t.Fatalf("redactBase leaked the token: %q", out)
	}
	if !strings.Contains(out, "<redacted>") {
		t.Fatalf("redactBase did not insert the <redacted> marker: %q", out)
	}
	// The non-secret context (the error tail) must survive so logs stay useful.
	if !strings.Contains(out, "connection refused") {
		t.Fatalf("redactBase dropped the useful error context: %q", out)
	}

	// Empty apiBase: nothing to redact -> unchanged.
	if got := redactBase(in, ""); got != in {
		t.Fatalf("redactBase with empty apiBase changed the string: %q", got)
	}
}

// captureLogger collects every formatted log line so a test can assert no line carries
// the token.
type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (c *captureLogger) logf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *captureLogger) joined() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

// TestRun_PollErrorLogIsTokenFree is the FINDING-1 regression test. With a token-bearing
// apiBase and a transport that always fails, http.Client.Do wraps the failure in a
// *url.Error whose message contains the request URL (token included). The loop logs that
// fault — and this test proves the logged line does NOT contain the token. Before the
// redaction fix it FAILS, with the token visible in the captured log.
func TestRun_PollErrorLogIsTokenFree(t *testing.T) {
	// A transport that always fails. http.Client.Do wraps the error in a *url.Error that
	// embeds the request URL (built from the token-bearing apiBase) — exactly the production
	// leak path. No network leaves the test.
	client := &http.Client{Transport: failingRoundTripper{}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cap := &captureLogger{}
	offsets := newOffsetStore(t)
	h := &recordingHandler{}
	sr := &sleepRecorder{}
	// Cancel after the first fault is logged so Run returns promptly (one fault is enough
	// to prove the leak is closed).
	var once sync.Once
	logfWithCancel := func(format string, args ...any) {
		cap.logf(format, args...)
		once.Do(cancel)
	}

	g := NewGateway(fakeAPIBase, offsets, h,
		WithHTTPClient(client),
		WithLogger(logfWithCancel),
		WithSleep(sr.sleep),
		WithBackoff(fastBackoff()),
		WithPollTimeout(0),
	)

	_ = g.Run(ctx)

	got := cap.joined()
	if got == "" {
		t.Fatal("no log line captured — the transport fault was not logged")
	}
	if strings.Contains(got, fakeToken) {
		t.Fatalf("FINDING 1 LEAK: gateway poll-error log line contains the bot token:\n%s", got)
	}
}

// failingRoundTripper always returns an error, so http.Client.Do produces a *url.Error
// that carries the request URL (which embeds the bot token via apiBase).
type failingRoundTripper struct{}

func (failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp 127.0.0.1:0: connect: connection refused")
}
