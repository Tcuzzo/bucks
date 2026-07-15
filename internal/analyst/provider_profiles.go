package analyst

import (
	"fmt"
	"sort"
	"strings"
)

// ProviderProfile is a named, OpenAI-compatible provider: its canonical base URL
// and a sensible default chat model. It carries NO key — every END-USER supplies
// their OWN free key (the build embeds no shared secret). A profile + a user key +
// an optional model override is all NewOpenAICompatBackend needs.
//
// The free default is NVIDIA NIM ("nvidia"): a user with no Ollama and no paid key
// can get a free, no-credit-card nvapi-... key at build.nvidia.com (a ~2-minute
// signup) and run the Nemotron Nano model. Groq, Cerebras, and OpenRouter are
// included as drop-in fallbacks: the SAME OpenAI chat shape, just a different base
// URL + key + model.
type ProviderProfile struct {
	// Name is the stable provider id used as the backend Name() and the
	// BUCKS_CHAT_PROVIDER value.
	Name string
	// BaseURL is the provider's OpenAI-compatible base. It MAY end in /v1 or not;
	// OpenAICompatBackend.chatCompletionsURL normalizes either form.
	BaseURL string
	// DefaultModel is the model used when the caller gives no override. For NVIDIA
	// it is the free Nemotron Nano reasoning model.
	DefaultModel string
}

// Provider id constants — the canonical BUCKS_CHAT_PROVIDER values.
const (
	// ProviderNvidia is NVIDIA NIM — the FREE default brain (no credit card).
	ProviderNvidia = "nvidia"
	// ProviderGroq is Groq's OpenAI-compatible API.
	ProviderGroq = "groq"
	// ProviderCerebras is Cerebras' OpenAI-compatible API.
	ProviderCerebras = "cerebras"
	// ProviderOpenRouter is OpenRouter's OpenAI-compatible aggregator.
	ProviderOpenRouter = "openrouter"
)

// nemotronNanoModel is NVIDIA NIM's best free chat/reasoning model as of 2026 — the
// model a key-less, Ollama-less user runs for free. Named once so the default and
// the docs/tests reference a single source of truth.
const nemotronNanoModel = "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning"

// providerProfiles is the canonical table of supported OpenAI-compatible providers.
// It is private; callers reach it through ProviderProfileByName so an unknown name
// is a clear error, not a zero-value profile that would later fail with an opaque
// "baseURL not configured".
var providerProfiles = map[string]ProviderProfile{
	ProviderNvidia: {
		Name:         ProviderNvidia,
		BaseURL:      "https://integrate.api.nvidia.com/v1",
		DefaultModel: nemotronNanoModel,
	},
	ProviderGroq: {
		Name:         ProviderGroq,
		BaseURL:      "https://api.groq.com/openai/v1",
		DefaultModel: "llama-3.3-70b-versatile",
	},
	ProviderCerebras: {
		Name:         ProviderCerebras,
		BaseURL:      "https://api.cerebras.ai/v1",
		DefaultModel: "llama-3.3-70b",
	},
	ProviderOpenRouter: {
		Name:         ProviderOpenRouter,
		BaseURL:      "https://openrouter.ai/api/v1",
		DefaultModel: "meta-llama/llama-3.3-70b-instruct:free",
	},
}

// ProviderProfileByName returns the named provider profile. The lookup is
// case-insensitive and trims surrounding space (a user-typed env value). Aliases
// "nemotron" and "openai" both resolve to NVIDIA NIM (the free default), so the
// wizard/CLI can offer the friendly "Free (NVIDIA Nemotron)" label and the operator
// can type the generic "openai" while still landing on the free brain. An unknown
// name returns an error naming the supported set — never a silent zero profile.
func ProviderProfileByName(name string) (ProviderProfile, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	switch key {
	case "nemotron", "openai":
		// Friendly aliases for the free NVIDIA default.
		key = ProviderNvidia
	}
	p, ok := providerProfiles[key]
	if !ok {
		return ProviderProfile{}, fmt.Errorf("analyst: unknown provider %q (supported: %s)",
			name, strings.Join(SupportedProviders(), ", "))
	}
	return p, nil
}

// SupportedProviders returns the supported provider ids in a stable, sorted order
// (for help text and error messages). It does not include the aliases — only the
// canonical ids.
func SupportedProviders() []string {
	out := make([]string, 0, len(providerProfiles))
	for k := range providerProfiles {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// IsKnownProviderBaseURL reports whether baseURL matches one of BUCKS's hosted
// OpenAI-compatible provider profile URLs. These hosted endpoints require a
// per-user API key; custom/self-hosted endpoints may still intentionally omit one.
func IsKnownProviderBaseURL(baseURL string) bool {
	target := normalizeProviderBaseURL(baseURL)
	if target == "" {
		return false
	}
	for _, profile := range providerProfiles {
		if normalizeProviderBaseURL(profile.BaseURL) == target {
			return true
		}
	}
	return false
}

func normalizeProviderBaseURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.ToLower(strings.TrimSpace(baseURL)), "/")
	return strings.TrimSuffix(baseURL, "/v1")
}
