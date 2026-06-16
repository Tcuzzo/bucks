package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"bucks/internal/analyst"
)

// --- test doubles -----------------------------------------------------------

// echoBackend is a mock analyst.Backend that RECORDS the exact prompt it was given
// (so a test can prove what the Chatter sent the model — the threaded history and the
// persona) and returns a COMPACT, deterministic reply. The reply is intentionally
// short (not the whole prompt) so the assistant turn that gets appended to the bounded
// history does not smuggle the entire transcript back in — the ring-buffer behavior is
// asserted on the prompt (be.last()), not laundered through a giant echoed reply.
type echoBackend struct {
	name       string
	mu         sync.Mutex
	calls      int
	lastPrompt string
}

func (b *echoBackend) Name() string { return b.name }

func (b *echoBackend) Complete(_ context.Context, prompt string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.lastPrompt = prompt
	return fmt.Sprintf("reply-%d", b.calls), nil
}

func (b *echoBackend) last() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastPrompt
}

// fixedBackend returns a fixed reply (used for grounding tests where the reply text,
// not the echo, is what matters).
type fixedBackend struct {
	name  string
	reply string
	err   error
	calls int
}

func (b *fixedBackend) Name() string { return b.name }

func (b *fixedBackend) Complete(_ context.Context, _ string) (string, error) {
	b.calls++
	if b.err != nil {
		return "", b.err
	}
	return b.reply, nil
}

// capturingLogger records every Printf so a test can assert a downgrade was logged
// (visible), not silent.
type capturingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *capturingLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *capturingLogger) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func newTestChatter(t *testing.T, persona Persona, opts []Option, backends ...analyst.Backend) *Chatter {
	t.Helper()
	c, err := NewChatter(persona, backends, opts...)
	if err != nil {
		t.Fatalf("NewChatter: %v", err)
	}
	return c
}

// --- tests ------------------------------------------------------------------

// TestSay_MultiTurnThreadsHistory proves multi-turn context: turn 2's prompt
// contains turn 1's user text AND turn 1's assistant reply, so the model is given the
// running conversation, not just the latest line. The echo backend lets us read the
// exact prompt the model received.
func TestSay_MultiTurnThreadsHistory(t *testing.T) {
	be := &echoBackend{name: "echo"}
	c := newTestChatter(t, NewPersona(""), nil, be)

	r1, err := c.Say(context.Background(), "how are my trades doing today")
	if err != nil {
		t.Fatalf("Say turn 1: %v", err)
	}
	r2, err := c.Say(context.Background(), "and what's your read on the market")
	if err != nil {
		t.Fatalf("Say turn 2: %v", err)
	}

	p2 := be.last()
	// Turn 1's USER text must be threaded into turn 2's prompt.
	if !strings.Contains(p2, "how are my trades doing today") {
		t.Errorf("turn 2 prompt missing turn 1 user text; prompt:\n%s", p2)
	}
	// Turn 1's ASSISTANT reply (r1.Text == "reply-1") must also be threaded into
	// turn 2's prompt — proving the prior model reply, not just user lines, is carried.
	if !strings.Contains(p2, r1.Text) {
		t.Errorf("turn 2 prompt missing the prior assistant reply %q; prompt:\n%s", r1.Text, p2)
	}
	// The current user turn is present.
	if !strings.Contains(p2, "and what's your read on the market") {
		t.Errorf("turn 2 prompt missing the current user turn; prompt:\n%s", p2)
	}
	// The prior assistant turn is present in history (the conversation has 4 turns:
	// user1, asst1, user2, asst2 after both Says).
	if got := c.Conversation().Len(); got != 4 {
		t.Errorf("conversation length = %d, want 4 (u1,a1,u2,a2)", got)
	}
	if r1.Text == "" || r2.Text == "" {
		t.Fatal("replies must be non-empty")
	}
	// Sanity: the threaded transcript labels both speakers.
	if !strings.Contains(p2, "OWNER:") || !strings.Contains(p2, "BUCKS:") {
		t.Errorf("turn 2 prompt should render both OWNER and BUCKS turns; prompt:\n%s", p2)
	}
}

// TestSay_FailoverIsVisibleNotSilent is the no-silent-downgrade test: the primary
// backend errors, the secondary answers, the reply names the secondary AND the
// failover is recorded in BOTH the Reply.Failovers trail and the logger.
func TestSay_FailoverIsVisibleNotSilent(t *testing.T) {
	primary := &fixedBackend{name: "primary", err: errors.New("503 service unavailable")}
	secondary := &fixedBackend{name: "secondary", reply: "doing alright — flat for the day, nothing forced."}
	log := &capturingLogger{}
	c := newTestChatter(t, NewPersona(""), []Option{WithLogger(log)}, primary, secondary)

	r, err := c.Say(context.Background(), "how's it going")
	if err != nil {
		t.Fatalf("Say: %v", err)
	}
	if r.Backend != "secondary" {
		t.Errorf("Reply.Backend = %q, want secondary (failover)", r.Backend)
	}
	if !r.Downgraded() {
		t.Fatal("Reply.Downgraded() = false; expected the failover to be recorded")
	}
	if len(r.Failovers) != 1 {
		t.Fatalf("len(Failovers) = %d, want 1: %+v", len(r.Failovers), r.Failovers)
	}
	if r.Failovers[0].From != "primary" {
		t.Errorf("Failover.From = %q, want primary", r.Failovers[0].From)
	}
	if !strings.Contains(r.Failovers[0].Err, "503") {
		t.Errorf("Failover.Err = %q, want it to carry the underlying 503", r.Failovers[0].Err)
	}
	logged := log.joined()
	if !strings.Contains(logged, "primary") || !strings.Contains(strings.ToLower(logged), "failing over") {
		t.Errorf("failover not logged visibly; log was:\n%s", logged)
	}
	if primary.calls != 1 || secondary.calls != 1 {
		t.Errorf("calls: primary=%d secondary=%d, want 1/1", primary.calls, secondary.calls)
	}
}

// TestSay_PersonaAndVoiceInPrompt proves the persona (the immutable honesty rules)
// AND the owner's chosen voice string are both present in the prompt the model sees.
func TestSay_PersonaAndVoiceInPrompt(t *testing.T) {
	const voice = "calm, dry, west-Texas drawl, never rushed"
	be := &echoBackend{name: "echo"}
	c := newTestChatter(t, NewPersona(voice), nil, be)

	if _, err := c.Say(context.Background(), "talk to me"); err != nil {
		t.Fatalf("Say: %v", err)
	}
	p := be.last()
	// The base persona identity + a hard honesty rule must be present.
	for _, want := range []string{
		"You are BUCKS",
		"NEVER promise or imply profit",
		"DUAL REGISTER",
		"never invent account numbers", // case-insensitive check below
	} {
		if !strings.Contains(strings.ToLower(p), strings.ToLower(want)) {
			t.Errorf("prompt missing persona fragment %q; prompt:\n%s", want, p)
		}
	}
	// The owner's voice must be carried through verbatim.
	if !strings.Contains(p, voice) {
		t.Errorf("prompt missing owner voice %q; prompt:\n%s", voice, p)
	}
}

// TestSay_BoundedHistoryDropsOldest proves the ring buffer: with a bound of 4 turns,
// after several exchanges only the last 4 turns are threaded into the prompt — the
// oldest are dropped (latency/context guard), never an unbounded transcript.
func TestSay_BoundedHistoryDropsOldest(t *testing.T) {
	be := &echoBackend{name: "echo"}
	c := newTestChatter(t, NewPersona(""), []Option{WithHistoryBound(4)}, be)

	// Six user turns -> twelve total turns generated, but only 4 may be retained.
	msgs := []string{
		"FIRSTMARK alpha",
		"SECONDMARK bravo",
		"THIRDMARK charlie",
		"FOURTHMARK delta",
		"FIFTHMARK echo",
		"SIXTHMARK foxtrot",
	}
	for i, m := range msgs {
		if _, err := c.Say(context.Background(), m); err != nil {
			t.Fatalf("Say %d: %v", i, err)
		}
	}

	if got := c.Conversation().Len(); got != 4 {
		t.Fatalf("conversation length = %d, want 4 (bounded)", got)
	}
	p := be.last()
	// The earliest marks must have been evicted from the prompt.
	for _, gone := range []string{"FIRSTMARK", "SECONDMARK", "THIRDMARK"} {
		if strings.Contains(p, gone) {
			t.Errorf("bounded history leaked old turn %q into prompt; prompt:\n%s", gone, p)
		}
	}
	// The most recent user turn is present.
	if !strings.Contains(p, "SIXTHMARK") {
		t.Errorf("prompt missing the most recent turn; prompt:\n%s", p)
	}
}

// TestSay_GroundsAccountFacts is the GHOST honesty test: with a FactProvider giving
// {"pnl":"+125.50"}, a reply that states "+125.50" grounds VERIFIED, while a reply
// that states a DIFFERENT number ("+999.99") is flagged UNVERIFIED — a fabricated
// account number is never presented as truth. This reuses analyst.Ground.
func TestSay_GroundsAccountFacts(t *testing.T) {
	facts := FactFunc(func(context.Context) map[string]string {
		return map[string]string{"pnl": "+125.50", "mode": "paper"}
	})

	// Case A: the model states the REAL pnl -> the claim grounds Verified.
	truthful := &fixedBackend{name: "m", reply: "You're up +125.50 on paper today — solid, but it's simulated money."}
	cTrue := newTestChatter(t, NewPersona(""), []Option{WithFactProvider(facts)}, truthful)
	rTrue, err := cTrue.Say(context.Background(), "what's my P&L")
	if err != nil {
		t.Fatalf("Say truthful: %v", err)
	}
	if !hasVerifiedValue(rTrue, "125.50") {
		t.Errorf("a reply stating the REAL pnl 125.50 must ground Verified; claims=%+v", rTrue.Claims)
	}
	if len(rTrue.Unverified()) != 0 {
		t.Errorf("truthful reply should have no unverified figures; got %+v", rTrue.Unverified())
	}

	// Case B: the model fabricates a DIFFERENT number -> flagged Unverified.
	liar := &fixedBackend{name: "m", reply: "You're up +999.99 today, easy money."}
	cLie := newTestChatter(t, NewPersona(""), []Option{WithFactProvider(facts)}, liar)
	rLie, err := cLie.Say(context.Background(), "what's my P&L")
	if err != nil {
		t.Fatalf("Say liar: %v", err)
	}
	if hasVerifiedValue(rLie, "999.99") {
		t.Error("a fabricated number 999.99 must NOT ground Verified")
	}
	if !hasUnverifiedValue(rLie, "999.99") {
		t.Errorf("the fabricated 999.99 must be flagged Unverified; claims=%+v", rLie.Claims)
	}
}

// TestSay_NoBackendErrorsCleanly proves Say with NO backend is impossible to
// construct (NewChatter rejects it) — the no-backend case returns a clear error and
// never panics. (A Chatter cannot exist with zero backends, so this guards the
// constructor, the only entry to a backend-less state.)
func TestSay_NoBackendErrorsCleanly(t *testing.T) {
	_, err := NewChatter(NewPersona(""), nil)
	if err == nil {
		t.Fatal("NewChatter with no backends must return a clear error, got nil")
	}
	if !strings.Contains(err.Error(), "backend") {
		t.Errorf("error should mention the missing backend, got: %v", err)
	}
	// Also prove a total runtime failure (every backend errors) returns a clear error
	// and does NOT fabricate a reply or panic.
	bad := &fixedBackend{name: "bad", err: errors.New("connection refused")}
	c := newTestChatter(t, NewPersona(""), nil, bad)
	r, err := c.Say(context.Background(), "hello")
	if err == nil {
		t.Fatal("Say with all backends failing must return an error, got nil")
	}
	if r.Text != "" {
		t.Errorf("failed Say must not fabricate a reply, got %q", r.Text)
	}
	// The failed user turn is dropped so a retry is clean (no orphaned turn).
	if c.Conversation().Len() != 0 {
		t.Errorf("failed Say should drop the unanswered user turn; len=%d", c.Conversation().Len())
	}
}

// TestSay_NoFactsForbidsNumbers proves that with NO FactProvider the prompt tells the
// model it has no account facts and must not state any — the honest default when the
// system has nothing to ground against.
func TestSay_NoFactsForbidsNumbers(t *testing.T) {
	be := &echoBackend{name: "echo"}
	c := newTestChatter(t, NewPersona(""), nil, be)
	if _, err := c.Say(context.Background(), "how much am I up"); err != nil {
		t.Fatalf("Say: %v", err)
	}
	p := strings.ToLower(be.last())
	if !strings.Contains(p, "none available") {
		t.Errorf("with no facts, prompt must say none are available; prompt:\n%s", be.last())
	}
}

// TestConversation_BoundEnforcedDirectly is a focused unit test on the ring buffer:
// appending past the bound keeps exactly the last N, oldest-first, and never grows.
func TestConversation_BoundEnforcedDirectly(t *testing.T) {
	cv := NewConversation(3)
	for i := 0; i < 10; i++ {
		cv.Append(Turn{Role: RoleUser, Text: fmt.Sprintf("t%d", i)})
	}
	if cv.Len() != 3 {
		t.Fatalf("Len = %d, want 3", cv.Len())
	}
	got := cv.Turns()
	want := []string{"t7", "t8", "t9"}
	for i := range want {
		if got[i].Text != want[i] {
			t.Errorf("turn[%d].Text = %q, want %q", i, got[i].Text, want[i])
		}
	}
	// A non-positive bound falls back to the default (a bound is never removable).
	if NewConversation(0).MaxTurns() != DefaultHistoryTurns {
		t.Errorf("zero bound should fall back to default %d", DefaultHistoryTurns)
	}
}

// TestGround_IgnoresGeneralNumbers is the PRECISION fix: a reply full of ordinary
// numbers — list markers, a general risk rule, a textbook ATR multiple, a casual
// "5 grand" — must produce ZERO unverified flags, because NONE of them is a statement
// about the owner's real account. Only an account-fact context makes a number a claim.
//
// This test also proves the BITE: with the OLD extract-every-number logic these would
// all be flagged. We assert that here against the old behavior re-implemented locally
// (oldExtractEveryNumber) so the regression can never silently come back: old flags > 0,
// new flags == 0.
func TestGround_IgnoresGeneralNumbers(t *testing.T) {
	facts := map[string]string{"pnl": "+125.50"}
	reply := "Here's the plan. Rule 1: never average down. " +
		"Rule 2: risk 2% per trade, no more. Rule 3: set a stop at 1.5x ATR. " +
		"If you only have 5 grand to trade with, size down — survive first."

	// NEW logic: none of these numbers is an account claim, so nothing is flagged.
	claims := groundReply(reply, facts)
	newUnverified := 0
	for _, c := range claims {
		if !c.Verified {
			newUnverified++
		}
	}
	if newUnverified != 0 {
		t.Errorf("general/illustrative numbers must NOT be flagged; got %d unverified: %+v",
			newUnverified, claims)
	}

	// BITE PROOF: the OLD logic (flag every number-ish token that doesn't match a fact)
	// would have flagged several of these. Prove the old behavior actually bit, so this
	// test demonstrates a real regression was fixed (not a no-op).
	oldUnverified := oldExtractEveryNumber(reply, facts)
	if oldUnverified == 0 {
		t.Fatalf("bite proof broken: the OLD extract-every-number logic should flag the "+
			"general numbers (1,2,2,3,1.5,5...), got %d", oldUnverified)
	}
	t.Logf("bite proof: OLD logic flagged %d general number(s) as unverified; NEW logic flags %d",
		oldUnverified, newUnverified)
}

// oldExtractEveryNumber re-implements the PRE-FIX extraction (every numeric token that
// does not match a supplied fact becomes an unverified claim) so the test can prove the
// new logic actually changed behavior on the same input. It returns the count of
// unverified flags the old logic would have produced.
func oldExtractEveryNumber(reply string, facts map[string]string) int {
	// Reverse index value -> key, mirroring the old groundReply.
	valToKey := map[string]string{}
	for k, v := range facts {
		if core := numericCore(v); core != "" {
			if _, seen := valToKey[core]; !seen {
				valToKey[core] = k
			}
		}
	}
	// Old numericTokens: split on non-figure runes and core each run.
	var tokens []string
	for _, raw := range strings.FieldsFunc(reply, func(r rune) bool {
		return !(r >= '0' && r <= '9') && r != '.' && r != ',' &&
			r != '+' && r != '-' && r != '$' && r != '%'
	}) {
		if core := numericCore(raw); core != "" {
			tokens = append(tokens, core)
		}
	}
	unverified := 0
	seen := map[string]bool{}
	for _, tok := range tokens {
		if seen[tok] {
			continue
		}
		seen[tok] = true
		if _, ok := valToKey[tok]; !ok {
			unverified++ // old logic: a number matching no fact is flagged unverified
		}
	}
	return unverified
}

// TestGround_FlagsFalseAccountClaim keeps the SAFETY property: a number presented as
// the owner's account state ("you're up +999.99 on the day") that the real facts do NOT
// back MUST still be flagged unverified — the precision fix does not relax this.
func TestGround_FlagsFalseAccountClaim(t *testing.T) {
	facts := map[string]string{"pnl": "+125.50"}
	claims := groundReply("you're up +999.99 on the day, easy money", facts)

	flagged := false
	for _, c := range claims {
		if !c.Verified && strings.Contains(c.Text, "999.99") {
			flagged = true
		}
		if c.Verified && strings.Contains(c.Text, "999.99") {
			t.Errorf("a false account number 999.99 must NOT verify; claim=%+v", c)
		}
	}
	if !flagged {
		t.Errorf("a false account claim (999.99 vs real +125.50) must be flagged unverified; claims=%+v", claims)
	}
}

// TestGround_VerifiesTrueAccountClaim proves a TRUE account restatement still grounds:
// facts say pnl=+125.50, the reply states "your P&L is +125.50" -> verified, and there
// are zero unverified figures.
func TestGround_VerifiesTrueAccountClaim(t *testing.T) {
	facts := map[string]string{"pnl": "+125.50"}
	claims := groundReply("your P&L is +125.50 on paper right now", facts)

	verified := false
	unverified := 0
	for _, c := range claims {
		if c.Verified && strings.Contains(c.Text, "125.50") {
			verified = true
		}
		if !c.Verified {
			unverified++
		}
	}
	if !verified {
		t.Errorf("the true account figure 125.50 must ground Verified; claims=%+v", claims)
	}
	if unverified != 0 {
		t.Errorf("a truthful account reply must have zero unverified figures; got %d: %+v", unverified, claims)
	}
}

// --- helpers ----------------------------------------------------------------

func hasVerifiedValue(r Reply, val string) bool {
	for _, c := range r.Claims {
		if c.Verified && strings.Contains(c.Text, val) {
			return true
		}
	}
	return false
}

func hasUnverifiedValue(r Reply, val string) bool {
	for _, c := range r.Claims {
		if !c.Verified && strings.Contains(c.Text, val) {
			return true
		}
	}
	return false
}
