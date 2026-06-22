package main

import (
	"context"
	"os"
	"strings"

	"bucks/internal/analyst"
	"bucks/internal/chat"
	"bucks/internal/secrets"
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

// codexAvailable reports whether the codex CLI is usable (installed + on PATH) — the
// "sign in with ChatGPT, no API key" path. It is a package var so tests can simulate a
// customer WITH or WITHOUT codex deterministically.
var codexAvailable = analyst.CodexAvailable

// configChatBackends resolves the ORDERED chat backends FROM THE SAVED SETUP (not env),
// primary first, so a user can talk to BUCKS right after the wizard with zero env vars. It
// returns an empty list (NOT an error) when the saved config yields nothing usable, so the
// caller can fall back to env or open the dashboard read-only — never a fabricated backend.
//
//   - LLMNemotronFree -> the FREE NVIDIA NIM (Nemotron) backend, keyed by the saved
//     nvapi-... key. The universal no-paid-key, no-Ollama, works-for-any-customer path.
//   - LLMCloudKey     -> the Ollama-style CloudKeyBackend, keyed by the saved key.
//   - LLMOAuthGPT     -> ChatGPT via the locally-installed codex CLI (the owner's ChatGPT
//     login, no API key). Built ONLY when codex is available; otherwise empty so the
//     dashboard guides the owner to the free brain — a customer without codex is never
//     stranded on a dead default.
//   - LLMBoth         -> ChatGPT (codex) PRIMARY + the saved cloud key as FALLBACK, in that
//     order, so a codex hiccup or quota limit fails over to the customer's own key.
//
// Construction is OFFLINE (no network/codex call); only a later Say runs a backend.
func configChatBackends(r tui.SetupResult) ([]analyst.Backend, error) {
	key := strings.TrimSpace(r.LLMKey)
	switch r.LLM {
	case tui.LLMNemotronFree:
		if key == "" {
			return nil, nil
		}
		profile, err := analyst.ProviderProfileByName("nemotron")
		if err != nil {
			return nil, err
		}
		return []analyst.Backend{analyst.NewOpenAICompatBackend(profile, key, "")}, nil
	case tui.LLMCloudKey:
		if key == "" {
			return nil, nil
		}
		return []analyst.Backend{analyst.NewCloudKeyBackend("chat-cloud", cloudKeyBaseURL, key, "", nil)}, nil
	case tui.LLMOAuthGPT:
		if codexAvailable() {
			return []analyst.Backend{analyst.NewCodexBackend("oauth-gpt", "", nil)}, nil
		}
		return nil, nil
	case tui.LLMBoth:
		var backends []analyst.Backend
		if codexAvailable() {
			backends = append(backends, analyst.NewCodexBackend("oauth-gpt", "", nil))
		}
		if key != "" {
			backends = append(backends, analyst.NewCloudKeyBackend("chat-cloud", cloudKeyBaseURL, key, "", nil))
		}
		return backends, nil // may be empty -> graceful
	default:
		return nil, nil
	}
}

// configChatBackend returns the single PRIMARY backend from the saved config (or nil when
// none) — the thin seam tests assert on. configChatBackends is the full ordered list.
func configChatBackend(r tui.SetupResult) (analyst.Backend, error) {
	backends, err := configChatBackends(r)
	if err != nil {
		return nil, err
	}
	if len(backends) == 0 {
		return nil, nil
	}
	return backends[0], nil
}

// cloudKeyBaseURL is the default Ollama-cloud endpoint for the saved cloud-key path.
// It matches the env path's expectation that the cloud-key backend speaks the
// Ollama /api/generate shape; the env path requires the URL explicitly, while the
// config path defaults it so a user who picked "cloud key" in setup can chat with
// just their key.
const cloudKeyBaseURL = "https://ollama.com"

// configChatter builds the dashboard's Chatter. An explicit environment provider or
// endpoint overrides the saved setup for compatibility; otherwise encrypted saved
// settings are the default. When neither yields a backend it returns (nil, nil).
func configChatter(r tui.SetupResult) (*chat.Chatter, error) {
	if envChatConfigured() {
		return envChatter()
	}
	backends, err := configChatBackends(r)
	if err != nil {
		return nil, err
	}
	if len(backends) == 0 {
		// A key by itself is not a complete environment override. Preserve the legacy
		// fallback behavior for any unusual environment combination that can still
		// construct a backend.
		return envChatter()
	}
	persona := chat.NewPersona("")
	// The full ordered list (e.g. codex primary + cloud-key fallback for "both") so a
	// downgrade fails over and is recorded, never a silent single-backend swap.
	return chat.NewChatter(persona, backends)
}

// envChatConfigured reports whether the user explicitly selected an environment
// endpoint/provider. A key alone is incomplete and must not hide a working saved setup.
func envChatConfigured() bool {
	return strings.TrimSpace(os.Getenv(envChatProvider)) != "" || strings.TrimSpace(os.Getenv(envChatBaseURL)) != ""
}

// runtimeChatBackends is the shared resolver for standalone chat, summary, research,
// and read commands. Explicit environment configuration wins for compatibility;
// otherwise the encrypted saved setup is the default.
func runtimeChatBackends(configPath, passphrase string, secretOpts ...secrets.Option) ([]analyst.Backend, error) {
	if envChatConfigured() {
		backend, err := envChatBackend()
		if err != nil || backend == nil {
			return nil, err
		}
		return []analyst.Backend{backend}, nil
	}
	if !configExists(configPath) {
		return nil, nil
	}
	r, _, err := loadSetupWithUnlock(configPath, passphrase, secretOpts...)
	if err != nil {
		return nil, err
	}
	return configChatBackends(r)
}

func runtimeChatter(configPath, passphrase string, secretOpts ...secrets.Option) (*chat.Chatter, error) {
	backends, err := runtimeChatBackends(configPath, passphrase, secretOpts...)
	if err != nil || len(backends) == 0 {
		return nil, err
	}
	persona := chat.NewPersona(strings.TrimSpace(os.Getenv(envChatVoice)))
	return chat.NewChatter(persona, backends)
}
