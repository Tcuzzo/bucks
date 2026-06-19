package channel

import "strings"

// redact.go — secret hygiene for the live Telegram channel, mirroring the gateway's sibling
// (internal/gateway/redact.go). The bot token lives IN apiBase
// (https://api.telegram.org/bot<token>), so any *url.Error from http.Client.Do embeds the
// full token in its message — and the trade loop returns and logs those errors on routine
// transient faults (a brief DNS hiccup, a connection reset). An un-redacted wrap would write
// the token — which grants full control of the operator's bot, the remote /halt surface — to
// the logs. redactBase scrubs the token-bearing base out of the string; redactedError applies
// it to a wrapped error while PRESERVING the chain so errors.Is (e.g. context.Canceled at
// shutdown) is unaffected.

// redactedBase keeps the URL shape (so a log line still reads as a Telegram URL) but carries
// no secret.
const redactedBase = "https://api.telegram.org/bot<redacted>"

// redactBase replaces every occurrence of the token-bearing apiBase in s with a token-free
// placeholder. An empty apiBase is a no-op.
func redactBase(s, apiBase string) string {
	if apiBase == "" {
		return s
	}
	return strings.ReplaceAll(s, apiBase, redactedBase)
}

// redactedError wraps err so its message has the token-bearing apiBase scrubbed, while still
// Unwrapping to the original error — so errors.Is/As classification (context cancellation,
// deadline) keeps working on the redacted error.
type redactedError struct {
	err     error
	apiBase string
}

// Error returns the underlying error's message with the bot token redacted.
func (e *redactedError) Error() string { return redactBase(e.err.Error(), e.apiBase) }

// Unwrap preserves the original error for errors.Is/As.
func (e *redactedError) Unwrap() error { return e.err }
