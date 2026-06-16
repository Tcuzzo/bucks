// Package chat is BUCKS's conversational surface: the trader you can TALK TO in
// plain human language. You ask it how it's doing, what it's thinking, what your
// positions look like — and it answers like a real person, plain to a first-timer
// and technical to a pro, confident but HONEST about risk.
//
// Two invariants from the rest of BUCKS are carried in by construction here:
//
//   - NO SILENT MODEL DOWNGRADE. The Chatter reasons over an ORDERED list of
//     analyst.Backends with the same failover discipline as the analyst: when the
//     primary errors, the next backend answers and that downgrade is RECORDED in
//     the reply's Failovers trail and on a Logger — never a quiet swap. (We reuse
//     analyst.Backend so OAuth-GPT / cloud-key / MiniMax are interchangeable and
//     the live test points at a real model with zero new HTTP code.)
//   - NO FABRICATED ACCOUNT FACTS (GHOST honesty). When the chat states a number
//     about the account (P&L, a position size), it must be backed by REAL facts the
//     system actually has. A FactProvider supplies those facts; the persona
//     instructs the model to state ONLY supplied account facts; and after the reply
//     comes back, any fact-bearing claim is run through analyst.Ground-style
//     checking so a fabricated number is FLAGGED as unverified, not presented as
//     truth. BUCKS never promises profit and is always honest that paper != live.
//
// The conversation history is a BOUNDED ring buffer (the latency/context lesson):
// only the last N turns are kept and threaded into the prompt, so a long chat can
// never balloon the context window or the per-turn latency.
//
// Nothing here makes a network call in the default test suite: the Backend is a
// thin Complete(ctx, prompt) contract driven by mocks. A real-model REPL lives in
// cmd/bucks behind env config, and a real-network smoke test exists only behind
// the `chat_live` build tag.
package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"bucks/internal/analyst"
)

// Role is who spoke a turn. Only "user" and "assistant" are valid; the persona is
// injected separately, never as a turn, so it can never be dropped by the ring
// buffer.
const (
	// RoleUser is a turn the owner typed.
	RoleUser = "user"
	// RoleAssistant is a turn BUCKS replied.
	RoleAssistant = "assistant"
)

// DefaultHistoryTurns is how many recent turns the conversation keeps when no
// explicit bound is given. It is deliberately small: enough to hold context across
// a back-and-forth, bounded so latency/context never grows without limit.
const DefaultHistoryTurns = 16

// Turn is one line of the conversation. Role is RoleUser or RoleAssistant; Text is
// what was said. Money/facts are never stored as float64 here — Text is plain
// language and any numbers come from grounded facts, not invented in this layer.
type Turn struct {
	Role string
	Text string
}

// Conversation is a BOUNDED ring buffer of the most recent turns. It never grows
// past maxTurns: appending the (maxTurns+1)th turn drops the oldest. This is the
// latency/context guard — a marathon chat keeps O(maxTurns) memory and prompt size,
// not O(all history).
type Conversation struct {
	turns    []Turn
	maxTurns int
}

// NewConversation builds an empty conversation bounded to maxTurns recent turns. A
// non-positive maxTurns falls back to DefaultHistoryTurns (a bound is mandatory —
// an unbounded history is never allowed).
func NewConversation(maxTurns int) *Conversation {
	if maxTurns <= 0 {
		maxTurns = DefaultHistoryTurns
	}
	return &Conversation{maxTurns: maxTurns}
}

// Append adds a turn, evicting the oldest if the buffer is full. The bound is
// enforced on every append so the invariant holds at all times, not just at read.
func (c *Conversation) Append(t Turn) {
	c.turns = append(c.turns, t)
	if len(c.turns) > c.maxTurns {
		// Drop the oldest turn(s). Re-slice onto a fresh backing array so the
		// dropped turns are not retained (no memory creep on a long session).
		over := len(c.turns) - c.maxTurns
		trimmed := make([]Turn, c.maxTurns)
		copy(trimmed, c.turns[over:])
		c.turns = trimmed
	}
}

// Turns returns a COPY of the current bounded history (oldest first), so a caller
// can read it without mutating the buffer.
func (c *Conversation) Turns() []Turn {
	out := make([]Turn, len(c.turns))
	copy(out, c.turns)
	return out
}

// Len is the number of turns currently held (never exceeds the bound).
func (c *Conversation) Len() int { return len(c.turns) }

// MaxTurns is the configured bound.
func (c *Conversation) MaxTurns() int { return c.maxTurns }

// FactProvider supplies the REAL, current account facts the chat is allowed to
// state as truth: positions, P&L, halted?, mode (paper/live). The returned map is
// fact-key -> value (e.g. {"pnl":"+125.50","mode":"paper"}). It is the ONLY source
// of account numbers the chat may present as fact — anything the model says that is
// not backed here is flagged unverified. A nil provider (or empty map) means the
// chat has no account facts and must not state any as truth.
type FactProvider interface {
	// Facts returns the current grounded account facts. The implementation pulls
	// from the live trader/broker state; this layer treats whatever it returns as
	// the ground truth and never invents beyond it.
	Facts(ctx context.Context) map[string]string
}

// FactFunc adapts a plain function to a FactProvider.
type FactFunc func(ctx context.Context) map[string]string

// Facts implements FactProvider.
func (f FactFunc) Facts(ctx context.Context) map[string]string { return f(ctx) }

// Reply is the result of one Say: the assistant's text plus the honesty/routing
// provenance the operator must see. Failovers lists every backend downgrade that
// happened producing this reply (empty when the primary answered) — visible, never
// silent. Claims are the fact-bearing assertions found in the reply, each grounded
// against the supplied facts (Verified=true only when backed by a real fact). A
// non-empty Unverified set means the reply contains a number BUCKS could NOT back —
// it is surfaced as unverified, never as established fact.
type Reply struct {
	// Text is the assistant's natural-language reply, as said.
	Text string
	// Backend is the model that PRODUCED the reply (the surviving backend after any
	// failover).
	Backend string
	// Failovers records every downgrade on the way to Backend (the
	// no-silent-downgrade trail).
	Failovers []analyst.Failover
	// Claims are the reply's fact-bearing claims, grounded against the facts.
	Claims []analyst.Claim
}

// Downgraded reports whether the reply was produced after at least one failover.
func (r Reply) Downgraded() bool { return len(r.Failovers) > 0 }

// Unverified returns the reply's claims that did NOT ground against real facts —
// the numbers BUCKS could not back. A non-empty result must be shown as unverified.
func (r Reply) Unverified() []analyst.Claim {
	var out []analyst.Claim
	for _, c := range r.Claims {
		if !c.Verified {
			out = append(out, c)
		}
	}
	return out
}

// Chatter is the conversational engine. It wraps an ORDERED list of
// analyst.Backends (primary first, the rest fallbacks — same failover model as the
// analyst), a Persona, the bounded Conversation, and an optional FactProvider for
// account grounding. It is the thing cmd/bucks's REPL and the Telegram surface call
// to "talk to BUCKS".
type Chatter struct {
	backends []analyst.Backend
	persona  Persona
	convo    *Conversation
	facts    FactProvider
	log      analyst.Logger
}

// Option configures a Chatter at construction (history bound, facts, logger).
type Option func(*Chatter)

// WithHistoryBound sets the bounded-history size (turns kept). A non-positive value
// keeps the default — the bound is never removable.
func WithHistoryBound(maxTurns int) Option {
	return func(c *Chatter) { c.convo = NewConversation(maxTurns) }
}

// WithFactProvider wires the real account-fact source used for grounding.
func WithFactProvider(fp FactProvider) Option {
	return func(c *Chatter) { c.facts = fp }
}

// WithLogger wires the failover logger (the human-visible echo of a downgrade). The
// structured trail in Reply.Failovers is always present regardless.
func WithLogger(l analyst.Logger) Option {
	return func(c *Chatter) {
		if l != nil {
			c.log = l
		}
	}
}

// NewChatter builds a Chatter over a persona and an ORDERED list of backends
// (primary first). At least one backend is required; an empty list is a programming
// error and is reported, not silently tolerated (mirrors analyst.New). History is
// bounded by default; pass WithHistoryBound to change the size.
func NewChatter(persona Persona, backends []analyst.Backend, opts ...Option) (*Chatter, error) {
	if len(backends) == 0 {
		return nil, errors.New("chat: at least one backend is required")
	}
	c := &Chatter{
		backends: backends,
		persona:  persona,
		convo:    NewConversation(DefaultHistoryTurns),
		log:      nopLogger{},
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// nopLogger is the default failover sink: the structured Reply.Failovers trail is
// always the asserted record; the logger is the human-facing echo.
type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// Conversation exposes the bounded history (read access for the dashboard/tests).
func (c *Chatter) Conversation() *Conversation { return c.convo }

// Say is the heart of the chat: it builds the prompt (persona + the current
// grounded facts + a compact rendering of recent history + the user's turn), calls
// the backends with VISIBLE failover, appends BOTH turns to the bounded history, and
// returns the reply with its routing + honesty provenance.
//
// The user's turn is appended to history BEFORE the model call so the model sees the
// turn it is answering rendered consistently with prior turns; the assistant turn is
// appended after a successful reply. If every backend fails, NOTHING is fabricated:
// the assistant turn is not appended and a clear joined error is returned (so the
// owner never sees a made-up reply, and the failed user turn is dropped so a retry
// is clean).
func (c *Chatter) Say(ctx context.Context, userText string) (Reply, error) {
	facts := c.currentFacts(ctx)

	// Render the prompt from a SNAPSHOT of history that already includes this user
	// turn, so turn N's prompt carries turns 1..N-1 (history threaded) plus the new
	// turn. We append the user turn first, then render.
	c.convo.Append(Turn{Role: RoleUser, Text: userText})
	prompt := c.buildPrompt(facts)

	var failovers []analyst.Failover
	var errs []error
	for i, b := range c.backends {
		out, err := b.Complete(ctx, prompt)
		if err != nil {
			fo := analyst.Failover{From: b.Name(), Err: err.Error()}
			failovers = append(failovers, fo)
			errs = append(errs, fmt.Errorf("backend %s: %w", b.Name(), err))
			if i+1 < len(c.backends) {
				c.log.Printf("chat: backend %q failed (%v) — failing over to %q",
					b.Name(), err, c.backends[i+1].Name())
			} else {
				c.log.Printf("chat: backend %q failed (%v) — no further backends",
					b.Name(), err)
			}
			continue
		}
		text := strings.TrimSpace(out)
		// Ground any fact-bearing claims in the reply against the REAL facts so a
		// fabricated number is flagged, not presented as truth.
		claims := groundReply(text, facts)
		if len(failovers) > 0 {
			c.log.Printf("chat: produced reply on fallback backend %q after %d failover(s)",
				b.Name(), len(failovers))
		}
		c.convo.Append(Turn{Role: RoleAssistant, Text: text})
		return Reply{
			Text:      text,
			Backend:   b.Name(),
			Failovers: failovers,
			Claims:    claims,
		}, nil
	}

	// Every backend failed — drop the just-appended user turn so a retry is clean,
	// and never fabricate a reply.
	c.dropLastUserTurn()
	return Reply{}, fmt.Errorf("chat: all %d backend(s) failed: %w",
		len(c.backends), errors.Join(errs...))
}

// currentFacts pulls the live account facts (empty when no provider is wired). The
// returned map is the ONLY account truth the chat may state.
func (c *Chatter) currentFacts(ctx context.Context) map[string]string {
	if c.facts == nil {
		return nil
	}
	return c.facts.Facts(ctx)
}

// dropLastUserTurn removes the most-recently-appended user turn after a total
// backend failure, so the bounded history does not carry an unanswered turn into the
// next attempt.
func (c *Chatter) dropLastUserTurn() {
	n := len(c.convo.turns)
	if n == 0 {
		return
	}
	if c.convo.turns[n-1].Role == RoleUser {
		c.convo.turns = c.convo.turns[:n-1]
	}
}

// buildPrompt composes the full prompt: the persona (with the owner's voice and the
// honesty rules), the REAL account facts the model is allowed to state, a compact
// rendering of the bounded recent history (which already includes the current user
// turn), and a final instruction. It is deterministic given identical inputs.
func (c *Chatter) buildPrompt(facts map[string]string) string {
	var b strings.Builder
	b.WriteString(c.persona.System())
	b.WriteString("\n")
	b.WriteString(renderFacts(facts))
	b.WriteString("CONVERSATION SO FAR:\n")
	for _, t := range c.convo.Turns() {
		fmt.Fprintf(&b, "%s: %s\n", speaker(t.Role), t.Text)
	}
	b.WriteString("BUCKS:")
	return b.String()
}

// speaker maps a Role to the human label used in the rendered transcript.
func speaker(role string) string {
	switch role {
	case RoleAssistant:
		return "BUCKS"
	default:
		return "OWNER"
	}
}

// renderFacts writes the account-facts block. When there are facts, it lists them
// and instructs the model that these are the ONLY account numbers it may state.
// When there are none, it says so explicitly so the model does not invent any.
func renderFacts(facts map[string]string) string {
	var b strings.Builder
	if len(facts) == 0 {
		b.WriteString("ACCOUNT FACTS: none available right now. " +
			"Do NOT state any specific account number (P&L, position size, balance) — " +
			"say you don't have it in front of you instead of guessing.\n")
		return b.String()
	}
	b.WriteString("ACCOUNT FACTS (the ONLY account numbers you may state as fact — " +
		"never invent or round beyond these):\n")
	for _, k := range sortedKeys(facts) {
		fmt.Fprintf(&b, "- %s = %s\n", k, facts[k])
	}
	return b.String()
}
