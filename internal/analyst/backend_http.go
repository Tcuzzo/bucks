package analyst

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultHTTPTimeout bounds a single backend call so a hung provider can never
// stall the analyst — it surfaces as an error and triggers failover (a stalled
// model is a failure to route around, not a thing to wait on).
const defaultHTTPTimeout = 30 * time.Second

// OAuTHGPTBackend is a thin HTTP client for an OAuth-authenticated ChatGPT-style
// chat-completions endpoint (the OAuth path in spec §4.7). It speaks the
// OpenAI-compatible `/chat/completions` request/response shape so it can be
// driven against an httptest server mirroring that shape in the default suite,
// and against the real endpoint only behind the `analyst_live` build tag.
//
// It carries NO embedded secret default and NO network call at construction: the
// token and base URL are injected, keeping the default test suite offline.
type OAuTHGPTBackend struct {
	name    string
	baseURL string // e.g. https://api.openai.com/v1 (or an httptest URL)
	token   string // OAuth bearer token (never logged, never defaulted)
	model   string
	client  *http.Client
}

// NewOAuTHGPTBackend builds an OAuth-GPT backend. name defaults to "oauth-gpt"
// when blank; an injected *http.Client (e.g. with a custom transport) is used if
// non-nil, else a client with the default timeout. baseURL and token are required
// at call time (Complete errors clearly if baseURL is empty).
func NewOAuTHGPTBackend(name, baseURL, token, model string, client *http.Client) *OAuTHGPTBackend {
	if name == "" {
		name = "oauth-gpt"
	}
	if model == "" {
		model = "gpt-4o-mini"
	}
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &OAuTHGPTBackend{name: name, baseURL: baseURL, token: token, model: model, client: client}
}

// Name is the stable identifier recorded in the View and failover log.
func (b *OAuTHGPTBackend) Name() string { return b.name }

// chatRequest / chatResponse mirror the OpenAI-compatible chat-completions shape.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Complete sends the prompt as a single user message and returns the assistant's
// content. A non-2xx status, a transport error, an API-level error field, or an
// empty/malformed body all return an error (which makes the analyst fail over) —
// never a fabricated completion.
func (b *OAuTHGPTBackend) Complete(ctx context.Context, prompt string) (string, error) {
	if b.baseURL == "" {
		return "", fmt.Errorf("%s: baseURL not configured", b.name)
	}
	reqBody := chatRequest{
		Model:    b.model,
		Messages: []chatMessage{{Role: "user", Content: prompt}},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("%s: marshal request: %w", b.name, err)
	}
	url := strings.TrimRight(b.baseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("%s: build request: %w", b.name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
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

// CloudKeyBackend is a thin HTTP client for an API-key-authenticated
// Ollama-cloud / MiniMax-style endpoint (the second §4.7 path). It speaks an
// Ollama-style `/api/generate` request/response shape (a single `response`
// string) so it is distinct from the OAuth-GPT chat shape and can be driven
// against an httptest server mirroring it. Like the GPT backend, it holds no
// secret default and makes no network call until Complete.
type CloudKeyBackend struct {
	name    string
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewCloudKeyBackend builds a cloud-key backend. name defaults to "cloud-key".
func NewCloudKeyBackend(name, baseURL, apiKey, model string, client *http.Client) *CloudKeyBackend {
	if name == "" {
		name = "cloud-key"
	}
	if model == "" {
		model = "qwen3.5:cloud"
	}
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &CloudKeyBackend{name: name, baseURL: baseURL, apiKey: apiKey, model: model, client: client}
}

// Name is the stable identifier recorded in the View and failover log.
func (b *CloudKeyBackend) Name() string { return b.name }

type generateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

type generateResponse struct {
	Response string `json:"response"`
	Error    string `json:"error,omitempty"`
}

// Complete sends the prompt to the generate endpoint and returns the response
// text. Errors (non-2xx, transport, API error field, empty body) return an error
// to trigger failover — no fabrication.
func (b *CloudKeyBackend) Complete(ctx context.Context, prompt string) (string, error) {
	if b.baseURL == "" {
		return "", fmt.Errorf("%s: baseURL not configured", b.name)
	}
	reqBody := generateRequest{Model: b.model, Prompt: prompt, Stream: false}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("%s: marshal request: %w", b.name, err)
	}
	url := strings.TrimRight(b.baseURL, "/") + "/api/generate"
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
	var gr generateResponse
	if err := json.Unmarshal(raw, &gr); err != nil {
		return "", fmt.Errorf("%s: decode response: %w", b.name, err)
	}
	if gr.Error != "" {
		return "", fmt.Errorf("%s: api error: %s", b.name, gr.Error)
	}
	if strings.TrimSpace(gr.Response) == "" {
		return "", fmt.Errorf("%s: empty completion", b.name)
	}
	return gr.Response, nil
}

// snippet returns a short, single-line excerpt of an error body for diagnostics
// (so an HTML/long error page does not flood the log/error chain).
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
