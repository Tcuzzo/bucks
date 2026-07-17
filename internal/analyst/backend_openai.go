package analyst

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// defaultOpenAITemperature / defaultOpenAIMaxTokens are conservative defaults for
// the OpenAI-compatible request. They are sent on every call so a provider that
// requires them (some do) never rejects the request, and so a free-tier model
// returns a bounded, complete answer instead of an unbounded stream. NVIDIA NIM's
// Nemotron expects temperature 1.0 per the provider's own examples.
//
// defaultOpenAIMaxTokens suits a SHORT answer (a market lean, a chat reply). It is
// NOT enough for a caller that needs a long structured payload out of a REASONING
// model: such a model spends its budget thinking BEFORE it emits the payload, so a
// tight budget returns an empty `content` (which Complete reports loudly as "empty
// completion" — never a fabricated answer). A caller with a bigger payload raises
// the budget explicitly with WithMaxTokens.
const (
	defaultOpenAITemperature = 1.0
	defaultOpenAIMaxTokens   = 1024
)

// Option customizes an OpenAICompatBackend at construction. Options are the only
// way to change a request default from outside the package, so every deviation from
// the conservative defaults is explicit at the call site.
type Option func(*OpenAICompatBackend)

// WithMaxTokens sets the completion budget (max_tokens) the backend sends. A
// non-positive value is ignored — the conservative default stands rather than a
// silently unbounded (or zero) request.
//
// Raise it when the caller needs a long structured payload from a reasoning model:
// the model's chain-of-thought is spent from the SAME budget as the answer, so a
// budget sized for the answer alone comes back empty.
func WithMaxTokens(n int) Option {
	return func(b *OpenAICompatBackend) {
		if n > 0 {
			b.maxTokens = n
		}
	}
}

// OpenAICompatBackend is a thin HTTP client for ANY OpenAI-compatible
// `/chat/completions` endpoint — the free-brain path (spec §16). It is the sibling
// of CloudKeyBackend (which speaks Ollama's /api/generate): same uniform
// analyst.Backend contract, same offline-by-construction discipline (no embedded
// secret, no network call until Complete), but it speaks the OpenAI chat shape so
// it can drive NVIDIA NIM (the free Nemotron model), Groq, Cerebras, OpenRouter,
// or any other OpenAI-compatible provider by swapping base URL + key + model.
//
// Each END-USER supplies their OWN free API key (e.g. a no-credit-card nvapi-... key
// from build.nvidia.com). The backend embeds NO shared key.
type OpenAICompatBackend struct {
	name        string
	baseURL     string // e.g. https://integrate.api.nvidia.com/v1 (with or without /v1)
	apiKey      string // Bearer key (never logged, never defaulted)
	model       string
	temperature float64
	maxTokens   int
	client      *http.Client
}

// NewOpenAICompatBackend builds an OpenAI-compatible backend from a ProviderProfile
// (which carries the base URL + a sensible default model), the END-USER's own API
// key, and an optional model override (empty => the profile's default model). name
// defaults to the profile name when blank. Optional Options (e.g. WithMaxTokens)
// adjust the request defaults; with none, the conservative defaults apply.
//
// Each user supplies their own free key — treat the call as best-effort: a free
// provider may rate-limit (HTTP 429), which Complete surfaces as an error so the
// analyst fails over rather than stalling. A caller wanting transparent retry on a
// 429 should wrap the backend with a small backoff at the routing layer; the
// backend itself stays a single, honest round trip (no hidden retry that could
// silently mask a degraded provider).
func NewOpenAICompatBackend(profile ProviderProfile, apiKey, modelOverride string, opts ...Option) *OpenAICompatBackend {
	name := profile.Name
	if name == "" {
		name = "openai-compat"
	}
	model := strings.TrimSpace(modelOverride)
	if model == "" {
		model = profile.DefaultModel
	}
	b := &OpenAICompatBackend{
		name:        name,
		baseURL:     profile.BaseURL,
		apiKey:      apiKey,
		model:       model,
		temperature: defaultOpenAITemperature,
		maxTokens:   defaultOpenAIMaxTokens,
		client:      &http.Client{Timeout: defaultHTTPTimeout},
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// newOpenAICompatBackendWithClient is the test/injection constructor: it takes an
// explicit base URL + key + model + *http.Client so the default suite can point the
// backend at an httptest server (and inject that server's client) with no network.
// It is unexported because production builds through NewOpenAICompatBackend (a
// profile); tests use this to control the transport.
func newOpenAICompatBackendWithClient(name, baseURL, apiKey, model string, client *http.Client) *OpenAICompatBackend {
	if name == "" {
		name = "openai-compat"
	}
	if model == "" {
		model = "test-model"
	}
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &OpenAICompatBackend{
		name:        name,
		baseURL:     baseURL,
		apiKey:      apiKey,
		model:       model,
		temperature: defaultOpenAITemperature,
		maxTokens:   defaultOpenAIMaxTokens,
		client:      client,
	}
}

// Name is the stable identifier recorded in the View and failover log.
func (b *OpenAICompatBackend) Name() string { return b.name }

// openAIChatRequest mirrors the OpenAI-compatible chat-completions request. It
// reuses analyst's chatMessage (the {role,content} pair) and adds the fields a free
// provider expects (temperature, max_tokens, stream:false).
type openAIChatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	Stream      bool          `json:"stream"`
}

// chatCompletionsURL resolves the full endpoint from the configured base URL,
// tolerating BOTH a base that already ends in /v1 (e.g.
// https://integrate.api.nvidia.com/v1) and one that does not (e.g.
// https://api.groq.com/openai), and tolerating a trailing slash either way. The
// result always ends in `.../v1/chat/completions` — the canonical OpenAI path —
// so a user who pastes either form hits the right endpoint.
func (b *OpenAICompatBackend) chatCompletionsURL() string {
	base := strings.TrimRight(strings.TrimSpace(b.baseURL), "/")
	// If the base already ends with /v1, append only /chat/completions.
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

// Complete sends the prompt as a single user message to the OpenAI-compatible
// chat-completions endpoint and returns the assistant's content. A non-2xx status,
// a transport error, an API-level error field, or an empty/malformed body all
// return an error (which makes the analyst fail over) — never a fabricated
// completion. The shape, error handling, and body-limit mirror OAuTHGPTBackend
// exactly; the only differences are the request defaults and the base-URL handling.
func (b *OpenAICompatBackend) Complete(ctx context.Context, prompt string) (string, error) {
	if strings.TrimSpace(b.baseURL) == "" {
		return "", fmt.Errorf("%s: baseURL not configured", b.name)
	}
	temp := b.temperature
	maxTok := b.maxTokens
	if maxTok <= 0 {
		maxTok = defaultOpenAIMaxTokens
	}
	reqBody := openAIChatRequest{
		Model:       b.model,
		Messages:    []chatMessage{{Role: "user", Content: prompt}},
		Temperature: temp,
		MaxTokens:   maxTok,
		Stream:      false,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("%s: marshal request: %w", b.name, err)
	}
	url := b.chatCompletionsURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("%s: build request: %w", b.name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if b.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: transport: %w", b.name, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("%s: read body: %w", b.name, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s: http %d: %s", b.name, resp.StatusCode, snippet(raw))
	}
	// Reuse chatResponse (choices[].message.content + optional error) — the same
	// OpenAI shape OAuTHGPTBackend decodes.
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("%s: decode response: %w", b.name, err)
	}
	if cr.Error != nil && cr.Error.Message != "" {
		return "", fmt.Errorf("%s: api error: %s", b.name, cr.Error.Message)
	}
	if len(cr.Choices) == 0 || strings.TrimSpace(cr.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("%s: empty completion", b.name)
	}
	return cr.Choices[0].Message.Content, nil
}
