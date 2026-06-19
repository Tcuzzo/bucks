package gateway

import "strings"

// redact.go — secret hygiene for the Telegram surfaces. The bot token lives IN the apiBase
// (https://api.telegram.org/bot<token>), so any error string Go builds from a request URL —
// most notably a *url.Error from http.Client.Do — embeds the full token. Those strings reach
// logs (the always-on poller backs off on transport faults routinely) and propagate to
// callers that wrap and return them. redactBase scrubs the token-bearing apiBase out of such
// a string before it is ever logged or wrapped, so the secret can never reach a log line or a
// returned error. It is a pure string transform — it does NOT alter the underlying error
// value, so fault classification (errors.Is, ctx-cancel detection) is unaffected.

// redactedBase is what a token-bearing apiBase is replaced with. It keeps the shape (so a log
// line still reads as a Telegram URL) while carrying no secret.
const redactedBase = "https://api.telegram.org/bot<redacted>"

// redactBase replaces every occurrence of apiBase (which contains the bot token) in s with a
// token-free placeholder. An empty apiBase is a no-op (nothing to redact). The non-secret
// context around the token (host, path, the underlying network error) is preserved so logs
// stay useful for debugging.
func redactBase(s, apiBase string) string {
	if apiBase == "" {
		return s
	}
	return strings.ReplaceAll(s, apiBase, redactedBase)
}
