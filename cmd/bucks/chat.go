package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"bucks/internal/analyst"
	"bucks/internal/chat"
)

// chatBackendEnv names the env vars that point the chat REPL at ANY
// OpenAI/Ollama-compatible endpoint, so the live test (and a real session) can talk
// to a real cloud model with zero new HTTP code (it reuses analyst's CloudKeyBackend,
// the Ollama-style /api/generate client). They are read at REPL start, never baked in.
const (
	// envChatBaseURL is the endpoint base (e.g. https://ollama.com or an OpenAI base).
	envChatBaseURL = "BUCKS_CHAT_BASEURL"
	// envChatKey is the API key / bearer token (may be empty for a local endpoint).
	envChatKey = "BUCKS_CHAT_KEY"
	// envChatModel is the model id (e.g. qwen3.5:cloud); a sensible default is used when blank.
	envChatModel = "BUCKS_CHAT_MODEL"
	// envChatVoice optionally sets the owner's preferred voice for the persona.
	envChatVoice = "BUCKS_CHAT_VOICE"
	// envChatProvider selects which backend KIND the chat/summary CLI builds:
	//   - "" / "ollama"                  -> CloudKeyBackend (Ollama /api/generate) — the existing default
	//   - "openai"/"nemotron"/"nvidia"   -> OpenAICompatBackend at NVIDIA NIM (the FREE Nemotron brain)
	//   - "groq"/"cerebras"/"openrouter" -> OpenAICompatBackend at that provider
	// When the provider is an OpenAI-compatible one and BUCKS_CHAT_BASEURL/_MODEL are
	// blank, the base URL + model default from the provider profile, so a user only
	// needs to paste their own free key in BUCKS_CHAT_KEY.
	envChatProvider = "BUCKS_CHAT_PROVIDER"
)

// runChatStdio is the production REPL wired to the real terminal. It resolves the
// backend from env (BUCKS_CHAT_BASEURL/_KEY/_MODEL) and drives the loop over os
// stdin/stdout.
func runChatStdio() error {
	return runChat(os.Stdin, os.Stdout, envChatter)
}

// envChatter builds the Chatter from the chat env vars, or returns (nil, nil) when no
// endpoint is configured (the no-backend case the REPL reports cleanly). It is the
// production factory; the default test injects a mock factory instead so it never
// touches the network.
//
// Configuration is "set enough to point at SOME model":
//   - An OpenAI-compatible provider (BUCKS_CHAT_PROVIDER=nemotron|nvidia|groq|...)
//     needs only the user's own key in BUCKS_CHAT_KEY — its base URL + model default
//     from the provider profile. This is the FREE path (NVIDIA Nemotron).
//   - The default Ollama path (provider unset/ollama) needs BUCKS_CHAT_BASEURL.
//
// If neither is configured, it returns (nil, nil) and the REPL prints clean guidance.
func envChatter() (*chat.Chatter, error) {
	backend, err := envChatBackend()
	if err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, nil
	}
	persona := chat.NewPersona(strings.TrimSpace(os.Getenv(envChatVoice)))
	return chat.NewChatter(persona, []analyst.Backend{backend})
}

// envChatBackend resolves the single analyst.Backend the chat/summary CLI should
// use from the BUCKS_CHAT_* env, honoring BUCKS_CHAT_PROVIDER. It returns (nil, nil)
// when nothing is configured (the clean no-backend case). It builds NO network
// connection — construction is offline; only a later Complete/Say hits the wire.
//
// This is the SINGLE backend-selection seam shared by chat and summary so the two
// commands can never drift in how they pick a provider.
func envChatBackend() (analyst.Backend, error) {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv(envChatProvider)))
	baseURL := strings.TrimSpace(os.Getenv(envChatBaseURL))
	key := strings.TrimSpace(os.Getenv(envChatKey))
	model := strings.TrimSpace(os.Getenv(envChatModel))

	switch provider {
	case "", "ollama":
		// Existing default: Ollama-style CloudKeyBackend. Unchanged behavior — it
		// still requires BUCKS_CHAT_BASEURL; without it, there is no backend.
		if baseURL == "" {
			return nil, nil
		}
		if key == "" && analyst.IsKnownProviderBaseURL(baseURL) {
			return nil, nil
		}
		return analyst.NewCloudKeyBackend("chat-cloud", baseURL, key, model, nil), nil
	default:
		// Any OpenAI-compatible provider (nemotron/nvidia/groq/cerebras/openrouter,
		// plus the openai alias). Base URL + model default from the profile, so the
		// user only needs to paste their own free key.
		profile, err := analyst.ProviderProfileByName(provider)
		if err != nil {
			return nil, err
		}
		if key == "" && (baseURL == "" || analyst.IsKnownProviderBaseURL(baseURL)) {
			return nil, nil
		}
		if baseURL != "" {
			// An explicit base URL overrides the profile (advanced/self-hosted).
			profile.BaseURL = baseURL
		}
		return analyst.NewOpenAICompatBackend(profile, key, model), nil
	}
}

// chatterFactory builds the Chatter to drive. Returning (nil, nil) means "no backend
// configured" — the REPL prints a clear message and exits 0 (never crashes). It is an
// injection seam so the default suite drives the REPL with a mock backend (no network).
type chatterFactory func() (*chat.Chatter, error)

// runChat opens the conversational REPL: it reads a line, calls Chatter.Say, and
// prints the reply, until EOF / "exit". The Chatter comes from the injected factory so
// the same loop serves a real session (env-configured cloud model) and the default
// test (mock backend). With NO backend (factory returns nil) it prints a clear message
// and exits 0 — it never crashes for lack of an LLM.
//
// in/out are injected so the REPL is testable without a real terminal.
func runChat(in io.Reader, out io.Writer, newChatter chatterFactory) error {
	chatter, err := newChatter()
	if err != nil {
		return fmt.Errorf("chat: %w", err)
	}
	if chatter == nil {
		fmt.Fprint(out, noBackendMessage("chat"))
		return nil
	}

	fmt.Fprintln(out, "BUCKS chat — talk to your trader. Type 'exit' or Ctrl-D to quit.")
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for {
		fmt.Fprint(out, "you> ")
		if !sc.Scan() {
			break // EOF
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			break
		}
		reply, err := chatter.Say(context.Background(), line)
		if err != nil {
			fmt.Fprintf(out, "bucks: %v\n", err)
			continue
		}
		fmt.Fprintf(out, "bucks> %s\n", reply.Text)
		// Surface routing + honesty provenance so a downgrade or an unbacked number is
		// never hidden from the owner.
		if reply.Downgraded() {
			fmt.Fprintf(out, "       (note: answered on fallback model %q after %d failover(s))\n",
				reply.Backend, len(reply.Failovers))
		}
		if un := reply.Unverified(); len(un) > 0 {
			fmt.Fprintf(out, "       (heads up: %d figure(s) above are NOT backed by your real account facts — treat as unverified)\n", len(un))
		}
	}
	fmt.Fprintln(out, "bucks: chat closed.")
	return nil
}

// newChatterFromEnv builds a chat.Chatter over a single CloudKeyBackend pointed at the
// configured endpoint. CloudKeyBackend is reused verbatim (the Ollama-style client) —
// no new HTTP code. The persona carries the owner's voice (or the default). It remains
// the explicit Ollama-path constructor used by the live chat smoke test.
func newChatterFromEnv(baseURL, key, model, voice string) (*chat.Chatter, error) {
	backend := analyst.NewCloudKeyBackend("chat-cloud", baseURL, key, model, nil)
	persona := chat.NewPersona(voice)
	return chat.NewChatter(persona, []analyst.Backend{backend})
}

// noBackendMessage is the shared, plain-English "no model configured yet" guidance
// for both `bucks chat` and `bucks summary`. It names the FREE path first (paste a
// no-credit-card NVIDIA key) so a user with no Ollama and no paid key has a one-line
// way to get going, then the Ollama path. command is "chat" or "summary".
func noBackendMessage(command string) string {
	return fmt.Sprintf("bucks %s: no LLM backend configured.\n"+
		"FREE option — get a no-credit-card key at build.nvidia.com (~2 min), then:\n"+
		"  %s=nemotron %s=nvapi-... bucks %s\n"+
		"Or point at any OpenAI/Ollama-compatible endpoint with %s (and optionally %s, %s).\n",
		command,
		envChatProvider, envChatKey, command,
		envChatBaseURL, envChatKey, envChatModel)
}
