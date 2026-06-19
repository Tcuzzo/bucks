package main

import (
	"context"
	"strings"

	"bucks/internal/analyst"
	"bucks/internal/chat"
	"bucks/internal/tui"
)

// chatterResponder adapts a *chat.Chatter to the tui.ChatResponder seam: the TUI
// wants a (string, error) reply, while Chatter.Say returns a rich chat.Reply (text +
// honesty/routing provenance). The adapter returns the reply's TEXT, and appends the
// honesty notes (a downgrade or an unverified-figure warning) so the dashboard surface
// stays honest exactly like the stdio REPL — the operator never sees a silent
// downgrade or an unbacked number presented as fact. A nil Chatter is reported as a
// nil ChatResponder by the builder, so this is never called on a nil chatter.
type chatterResponder struct{ c *chat.Chatter }

// Say implements tui.ChatResponder.
func (a chatterResponder) Say(ctx context.Context, text string) (string, error) {
	reply, err := a.c.Say(ctx, text)
	if err != nil {
		return "", err
	}
	out := reply.Text
	if reply.Downgraded() {
		out += "\n(note: answered on a fallback model after a downgrade)"
	}
	if un := reply.Unverified(); len(un) > 0 {
		out += "\n(heads up: a figure above is NOT backed by your real account facts — treat as unverified)"
	}
	return out, nil
}

// newChatResponder builds the dashboard's ChatResponder from the saved setup. It
// returns nil (chat disabled) when configChatter yields no Chatter — the dashboard
// then opens read-only with the configure-a-backend hint. It is the single seam
// launch uses to make the dashboard chat-ready from config.
func newChatResponder(r tui.SetupResult) (tui.ChatResponder, error) {
	ch, err := configChatter(r)
	if err != nil {
		return nil, err
	}
	if ch == nil {
		return nil, nil
	}
	return chatterResponder{c: ch}, nil
}

// configChatBackend resolves the chat backend FROM THE SAVED SETUP (not env), so a
// user can talk to BUCKS right after the wizard with zero env vars. It MIRRORS
// envChatBackend's provider logic, but the provider + key come from the persisted
// config:
//
//   - LLMNemotronFree -> the FREE NVIDIA NIM (Nemotron) OpenAICompat backend, keyed
//     by the saved nvapi-... key. This is the no-paid-key, no-Ollama path.
//   - LLMCloudKey     -> the Ollama-style CloudKeyBackend, keyed by the saved key.
//   - LLMOAuthGPT / LLMBoth -> there is no directly-usable HTTP backend buildable from
//     config here (OAuth-GPT is a browser/session flow, not a bearer key), so it
//     returns (nil, nil) — graceful, NEVER a fabricated backend.
//
// Construction is OFFLINE (no network call); only a later Say hits the wire. A
// missing key for a key-based path yields (nil, nil) so the dashboard still opens
// read-only with the "configure a backend" hint rather than a half-built backend.
func configChatBackend(r tui.SetupResult) (analyst.Backend, error) {
	key := strings.TrimSpace(r.LLMKey)
	switch r.LLM {
	case tui.LLMNemotronFree:
		if key == "" {
			return nil, nil
		}
		// The free brain: NVIDIA NIM via its provider profile (base URL + default
		// Nemotron model), keyed by the owner's saved free key. Reuses the SAME
		// OpenAICompat path the env "nemotron" provider uses, so the two can't drift.
		profile, err := analyst.ProviderProfileByName("nemotron")
		if err != nil {
			return nil, err
		}
		return analyst.NewOpenAICompatBackend(profile, key, ""), nil
	case tui.LLMCloudKey:
		if key == "" {
			return nil, nil
		}
		// The cloud-key path: the Ollama-style CloudKeyBackend, named "chat-cloud" to
		// match the env path. The base URL/model default inside the backend (the saved
		// config carries no separate base URL in this slice), so the user only needs
		// their key.
		return analyst.NewCloudKeyBackend("chat-cloud", cloudKeyBaseURL, key, "", nil), nil
	default:
		// OAuth-GPT / Both: no directly-usable HTTP backend from config. Graceful.
		return nil, nil
	}
}

// cloudKeyBaseURL is the default Ollama-cloud endpoint for the saved cloud-key path.
// It matches the env path's expectation that the cloud-key backend speaks the
// Ollama /api/generate shape; the env path requires the URL explicitly, while the
// config path defaults it so a user who picked "cloud key" in setup can chat with
// just their key.
const cloudKeyBaseURL = "https://ollama.com"

// configChatter builds the dashboard's Chatter from the SAVED setup, falling back to
// the env-configured chatter (BUCKS_CHAT_*) when the config yields no backend — so
// the existing env path keeps working. When NEITHER yields a backend it returns
// (nil, nil): the dashboard opens read-only with the "configure a backend" hint and
// NEVER blocks launch or fabricates a model.
func configChatter(r tui.SetupResult) (*chat.Chatter, error) {
	backend, err := configChatBackend(r)
	if err != nil {
		return nil, err
	}
	if backend == nil {
		// Config gave us nothing usable — honor the existing BUCKS_CHAT_* env path so a
		// power user's env still works from the dashboard.
		return envChatter()
	}
	persona := chat.NewPersona("")
	return chat.NewChatter(persona, []analyst.Backend{backend})
}
