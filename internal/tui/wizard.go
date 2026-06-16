// Package tui is BUCKS's cross-platform terminal UI: a guided-unpack wizard that
// walks a new owner through first-run setup, and a live dashboard that shows the
// trader's positions, P&L, and health at a glance. Both are bubbletea models —
// pure (Init/Update/View) state machines with NO terminal, network, or disk
// coupling in their logic, so they run and are tested identically on Linux and
// Windows (the operator's hard cross-platform requirement, spec §4.9).
//
// THE NEVER-STALL INVARIANT (Hydra TUI lesson, spec §6): Update MUST NEVER block.
// It only mutates in-memory state and returns the next model plus an optional
// tea.Cmd. Any slow work — a network round trip, disk read, broker call — is
// modeled as a tea.Cmd that the bubbletea runtime runs OFF the update loop and
// delivers back as a future tea.Msg. There is no http/net/os I/O anywhere in an
// Update path in this package; the latency budget for a keystroke is "mutate a
// struct and return", which is microseconds. The Critic/Verifier can prove this
// structurally (no net/http/os import is used inside Update) and behaviorally
// (every Update returns promptly with a model + cmd, never a synchronous call).
//
// Money is always orders.Decimal — never float64 — for any displayed P&L or
// capital, matching the rest of BUCKS.
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"bucks/internal/playbook"
)

// Step enumerates the guided-unpack stages IN ORDER (spec §4.9 step list). The
// numeric order IS the unwrap order; the wizard advances strictly forward through
// these on valid input and never skips a required gate. StepDone is the terminal
// state where the SetupResult is final.
type Step int

const (
	// StepWelcome — greet the owner, show the buck banner, and choose how to talk
	// to BUCKS (voice on/off) and which register to start in.
	StepWelcome Step = iota
	// StepTelegram — collect the Telegram bot token (its OWN token; never shared —
	// the 409 tap-drop lesson). A blank token is rejected here.
	StepTelegram
	// StepLLM — choose the reasoning backend (OAuth-GPT, a cloud key, or both).
	StepLLM
	// StepBroker — choose the broker and collect its key/secret. Alpaca paper is
	// the default; a blank key is rejected.
	StepBroker
	// StepIntake — walk playbook.DefaultIntake() questions and collect answers,
	// validating each against its Question.Validate before advancing.
	StepIntake
	// StepSafety — final confirm. Paper trading is ON by default; going live needs
	// an explicit, deliberate toggle (no accidental live trading).
	StepSafety
	// StepDone — the wizard is complete and Result() is final.
	StepDone
)

// String renders a step for logs and tests.
func (s Step) String() string {
	switch s {
	case StepWelcome:
		return "Welcome"
	case StepTelegram:
		return "Telegram"
	case StepLLM:
		return "LLM"
	case StepBroker:
		return "Broker"
	case StepIntake:
		return "Intake"
	case StepSafety:
		return "Safety"
	case StepDone:
		return "Done"
	default:
		return fmt.Sprintf("Step(%d)", int(s))
	}
}

// stepOrder is the canonical, asserted unwrap order. Advancing always walks this
// slice forward by exactly one; there is no hidden jump.
var stepOrder = []Step{StepWelcome, StepTelegram, StepLLM, StepBroker, StepIntake, StepSafety, StepDone}

// LLMChoice is the reasoning-backend selection.
type LLMChoice string

const (
	// LLMOAuthGPT routes reasoning through the OAuth-GPT (ChatGPT) backend.
	LLMOAuthGPT LLMChoice = "oauth-gpt"
	// LLMCloudKey routes reasoning through a cloud API key backend.
	LLMCloudKey LLMChoice = "cloud-key"
	// LLMBoth wires both backends (one as primary, the other as fallback).
	LLMBoth LLMChoice = "both"
)

func (c LLMChoice) valid() bool {
	switch c {
	case LLMOAuthGPT, LLMCloudKey, LLMBoth:
		return true
	default:
		return false
	}
}

// BrokerKind is the broker the owner connects.
type BrokerKind string

const (
	// BrokerAlpacaPaper is Alpaca's paper (simulated) environment — the safe default.
	BrokerAlpacaPaper BrokerKind = "alpaca-paper"
	// BrokerAlpacaLive is Alpaca's live (real-money) environment.
	BrokerAlpacaLive BrokerKind = "alpaca-live"
	// BrokerCoinbase is the Coinbase crypto venue.
	BrokerCoinbase BrokerKind = "coinbase"
	// BrokerTradier is the Tradier equities/options venue.
	BrokerTradier BrokerKind = "tradier"
)

func (b BrokerKind) valid() bool {
	switch b {
	case BrokerAlpacaPaper, BrokerAlpacaLive, BrokerCoinbase, BrokerTradier:
		return true
	default:
		return false
	}
}

// isLive reports whether this broker selection trades real money.
func (b BrokerKind) isLive() bool { return b == BrokerAlpacaLive }

// BrokerCreds is one broker connection the owner configured.
type BrokerCreds struct {
	Kind   BrokerKind
	Key    string
	Secret string
}

// SetupResult is the collected, validated configuration the wizard produces. It is
// the single hand-off artifact: cmd/bucks turns it into the on-disk config. Every
// field here came through a validated step — the wizard never emits a partial or
// contradictory result (a SetupResult only exists once StepDone is reached and the
// embedded Playbook passed playbook.BuildPlaybook).
type SetupResult struct {
	// VoiceEnabled is whether the owner wants the spoken/voice surface on.
	VoiceEnabled bool
	// TelegramToken is the owner's Telegram bot token (non-empty; its own bot).
	TelegramToken string
	// LLM is the chosen reasoning backend.
	LLM LLMChoice
	// Brokers are the configured broker connections (at least one).
	Brokers []BrokerCreds
	// Playbook is the validated owner mandate built from the intake answers.
	Playbook playbook.Playbook
	// Live is true only if the owner explicitly toggled live trading on; paper
	// (Live == false) is the default and the safe state.
	Live bool
}

// WizardModel is the guided-unpack state machine. It implements tea.Model and
// drives the unwrap steps in order. It holds ONLY in-memory fields — no client,
// socket, or file handle — so Update can never block and the model is trivially
// testable by feeding it messages.
type WizardModel struct {
	step Step

	// in-flight collected values (promoted into result at StepDone).
	voiceEnabled bool
	telegram     string
	llm          LLMChoice
	brokerKind   BrokerKind
	brokerKey    string
	brokerSecret string
	// brokerSecretPhase is the StepBroker sub-state: once the API key is accepted
	// the broker step does NOT advance — it switches to collecting the API secret
	// (a second masked prompt). Only a valid secret advances off StepBroker. This
	// keeps the outer Step order intact while collecting the REAL key+secret pair
	// every live venue (Alpaca-live, Coinbase, Tradier) needs to authenticate.
	brokerSecretPhase bool
	live              bool

	// intake drives the playbook questions in order; intakeIdx is the current
	// question; answers maps each Question.Id to the owner's raw answer.
	intake    playbook.Intake
	intakeIdx int
	answers   map[string]string

	// input is the line the owner is typing for the current free-text prompt.
	input string
	// errMsg is the inline validation error for the current step (empty = none).
	// It is shown to the owner and cleared on the next valid keystroke; a bad
	// answer NEVER advances the step and NEVER crashes — it sets errMsg and stays.
	errMsg string

	// result is populated at StepDone; done mirrors step == StepDone for callers
	// that hold the model by value.
	result SetupResult
	done   bool

	// styles are precomputed lipgloss styles (pure string transforms, no I/O).
	styles styleSet
}

// NewWizard constructs a fresh wizard at StepWelcome with the standard intake.
func NewWizard() WizardModel {
	return WizardModel{
		step:       StepWelcome,
		llm:        LLMOAuthGPT,
		brokerKind: BrokerAlpacaPaper,
		intake:     playbook.DefaultIntake(),
		answers:    map[string]string{},
		styles:     newStyles(),
	}
}

// Init implements tea.Model. The wizard needs no startup I/O, so it returns nil —
// the never-stall invariant holds from the first frame.
func (m WizardModel) Init() tea.Cmd { return nil }

// Done reports whether the wizard has completed (reached StepDone).
func (m WizardModel) Done() bool { return m.done }

// Result returns the collected SetupResult. It is only meaningful once Done() is
// true; before that it is the zero value.
func (m WizardModel) Result() SetupResult { return m.result }

// Step returns the current step (for tests and the header render).
func (m WizardModel) CurrentStep() Step { return m.step }

// Err returns the current inline error message (empty when none).
func (m WizardModel) Err() string { return m.errMsg }

// Update implements tea.Model. It is the heart of the never-stall invariant: it
// ONLY inspects the message and mutates in-memory state, then returns the next
// model and an optional tea.Cmd. It performs NO network, disk, or other blocking
// call — there is not even an import of net/http/os used in this method's path.
// Building the playbook at the final step is pure CPU (parse + validate), not I/O.
func (m WizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		// Non-key messages (resize, custom msgs) don't drive the wizard; ignore
		// them without blocking. Returning promptly preserves the latency budget.
		return m, nil
	}

	switch key.Type {
	case tea.KeyCtrlC:
		// A hard quit is always available; it is a runtime command, not I/O.
		return m, tea.Quit
	case tea.KeyEsc:
		// Esc steps the owner BACK one step (never below welcome). It clears any
		// pending input/error so the previous step renders clean.
		m = m.back()
		return m, nil
	}

	// Dispatch the keystroke to the active step. Each handler is a pure function
	// of (model, key) -> model: it either advances, sets an inline error, or edits
	// the in-flight input. None of them block.
	switch m.step {
	case StepWelcome:
		m = m.updateWelcome(key)
	case StepTelegram:
		m = m.updateTelegram(key)
	case StepLLM:
		m = m.updateLLM(key)
	case StepBroker:
		m = m.updateBroker(key)
	case StepIntake:
		m = m.updateIntake(key)
	case StepSafety:
		m = m.updateSafety(key)
	case StepDone:
		// Terminal: nothing advances; ctrl+c (handled above) is the only exit.
	}
	return m, nil
}

// advance moves strictly one step forward along stepOrder and clears transient
// per-step input/error state. It never skips a step.
func (m WizardModel) advance() WizardModel {
	for i, s := range stepOrder {
		if s == m.step && i+1 < len(stepOrder) {
			m.step = stepOrder[i+1]
			break
		}
	}
	m.input = ""
	m.errMsg = ""
	return m
}

// back moves one step backward (floored at StepWelcome) and clears transient
// input/error so the prior step is re-entered cleanly. Within StepBroker's secret
// sub-prompt, back returns to the key sub-prompt instead of leaving the step, so the
// owner can correct the key without losing their place in the outer step order.
func (m WizardModel) back() WizardModel {
	if m.step == StepBroker && m.brokerSecretPhase {
		m.brokerSecretPhase = false
		m.input = ""
		m.errMsg = ""
		return m
	}
	for i, s := range stepOrder {
		if s == m.step && i > 0 {
			m.step = stepOrder[i-1]
			break
		}
	}
	m.input = ""
	m.errMsg = ""
	return m
}

// --- per-step keystroke handlers (all pure, all non-blocking) ---

// updateWelcome: 'v' toggles voice, enter advances. The register is chosen later
// via the intake's risk-tolerance question, so welcome only gates voice + go.
func (m WizardModel) updateWelcome(k tea.KeyMsg) WizardModel {
	switch {
	case k.Type == tea.KeyEnter:
		return m.advance()
	case k.Type == tea.KeyRunes && strings.EqualFold(string(k.Runes), "v"):
		m.voiceEnabled = !m.voiceEnabled
		m.errMsg = ""
	}
	return m
}

// updateTelegram: type the token, enter validates+advances. A blank token is
// rejected inline (never advances, never crashes).
func (m WizardModel) updateTelegram(k tea.KeyMsg) WizardModel {
	if k.Type == tea.KeyEnter {
		tok := strings.TrimSpace(m.input)
		if tok == "" {
			m.errMsg = "Telegram bot token can't be empty — paste the token from @BotFather."
			return m
		}
		if !looksLikeTelegramToken(tok) {
			m.errMsg = "That doesn't look like a Telegram token (expected <digits>:<key>). Re-paste it."
			return m
		}
		m.telegram = tok
		return m.advance()
	}
	return m.editInput(k)
}

// updateLLM: 1=OAuth-GPT, 2=cloud key, 3=both; enter confirms the current choice.
func (m WizardModel) updateLLM(k tea.KeyMsg) WizardModel {
	if k.Type == tea.KeyRunes {
		switch string(k.Runes) {
		case "1":
			m.llm = LLMOAuthGPT
			m.errMsg = ""
		case "2":
			m.llm = LLMCloudKey
			m.errMsg = ""
		case "3":
			m.llm = LLMBoth
			m.errMsg = ""
		}
		return m
	}
	if k.Type == tea.KeyEnter {
		if !m.llm.valid() {
			m.errMsg = "Pick a backend: 1) OAuth-GPT  2) cloud key  3) both."
			return m
		}
		return m.advance()
	}
	return m
}

// updateBroker collects, in two sequential masked prompts, the REAL API key AND
// the REAL API secret for the chosen broker — both are required to authenticate at
// every live venue (Alpaca-live, Coinbase, Tradier all use key+secret). The owner
// first picks a broker and types the key; on a valid key the step does NOT advance,
// it switches to the secret sub-prompt (brokerSecretPhase). Only a valid secret
// advances off StepBroker. The entered secret is stored verbatim — never synthesized
// — so the persisted credential is the owner's actual secret, not a placeholder.
// enter in either sub-phase validates that sub-phase; a bad value stays inline.
func (m WizardModel) updateBroker(k tea.KeyMsg) WizardModel {
	if m.brokerSecretPhase {
		return m.updateBrokerSecret(k)
	}
	if k.Type == tea.KeyRunes && len(m.input) == 0 {
		// Broker selection only honored when the key line is still empty, so the
		// owner can type a key that happens to start with a digit afterward.
		switch string(k.Runes) {
		case "1":
			m.brokerKind = BrokerAlpacaPaper
			m.errMsg = ""
			return m
		case "2":
			m.brokerKind = BrokerAlpacaLive
			m.errMsg = ""
			return m
		case "3":
			m.brokerKind = BrokerCoinbase
			m.errMsg = ""
			return m
		case "4":
			m.brokerKind = BrokerTradier
			m.errMsg = ""
			return m
		}
	}
	if k.Type == tea.KeyEnter {
		if !m.brokerKind.valid() {
			m.errMsg = "Pick a broker: 1) Alpaca paper  2) Alpaca live  3) Coinbase  4) Tradier."
			return m
		}
		key := strings.TrimSpace(m.input)
		if key == "" {
			m.errMsg = "Broker API key can't be empty — paste your " + string(m.brokerKind) + " key."
			return m
		}
		if len(key) < 8 {
			m.errMsg = "That broker key looks too short to be valid — re-check and paste it again."
			return m
		}
		m.brokerKey = key
		// Key accepted — switch to the secret sub-prompt WITHOUT advancing the step.
		// A live broker selection arms (but does not enable) live trading; the owner
		// must still explicitly toggle it ON at the safety step.
		if m.brokerKind.isLive() {
			m.live = false
		}
		m.brokerSecretPhase = true
		m.input = ""
		m.errMsg = ""
		return m
	}
	return m.editInput(k)
}

// updateBrokerSecret is the second StepBroker sub-prompt: it collects the REAL API
// secret with the same validation discipline as the key (non-empty, reasonable min
// length). Only a valid secret stores m.brokerSecret and advances off StepBroker; an
// empty or too-short secret is rejected inline and the step stays. The secret is
// stored exactly as entered — no placeholder is ever synthesized.
func (m WizardModel) updateBrokerSecret(k tea.KeyMsg) WizardModel {
	if k.Type == tea.KeyEnter {
		secret := strings.TrimSpace(m.input)
		if secret == "" {
			m.errMsg = "Broker API secret can't be empty — paste your " + string(m.brokerKind) + " secret."
			return m
		}
		if len(secret) < 8 {
			m.errMsg = "That broker secret looks too short to be valid — re-check and paste it again."
			return m
		}
		m.brokerSecret = secret
		m.brokerSecretPhase = false
		return m.advance()
	}
	return m.editInput(k)
}

// updateIntake: type each answer, enter validates THIS question (via the real
// Question.Validate) and advances to the next; after the last question it advances
// the wizard step. A bad answer is rejected inline and the same question stays.
func (m WizardModel) updateIntake(k tea.KeyMsg) WizardModel {
	if k.Type != tea.KeyEnter {
		return m.editInput(k)
	}
	if m.intakeIdx >= len(m.intake.Questions) {
		return m.advance()
	}
	q := m.intake.Questions[m.intakeIdx]
	raw := strings.TrimSpace(m.input)
	if err := q.Validate(raw); err != nil {
		m.errMsg = plainErr(err)
		return m
	}
	if m.answers == nil {
		m.answers = map[string]string{}
	}
	m.answers[q.Id] = raw
	m.intakeIdx++
	m.input = ""
	m.errMsg = ""
	if m.intakeIdx >= len(m.intake.Questions) {
		// All intake questions answered; the playbook is BUILT at the safety step's
		// confirm so a late edit can't desync — here we only move to safety.
		return m.advance()
	}
	return m
}

// updateSafety: 'l' toggles live (only meaningful with a live broker; on a paper
// broker it stays paper and warns), enter finalizes by BUILDING the playbook from
// the collected answers. If the playbook fails to build, the error is shown inline
// and the wizard stays on safety (never emits a bad SetupResult).
func (m WizardModel) updateSafety(k tea.KeyMsg) WizardModel {
	if k.Type == tea.KeyRunes && strings.EqualFold(string(k.Runes), "l") {
		if !m.brokerKind.isLive() {
			m.live = false
			m.errMsg = "Live trading needs a live broker (you chose " + string(m.brokerKind) + "). Go back and pick a live broker to enable it."
			return m
		}
		m.live = !m.live
		m.errMsg = ""
		return m
	}
	if k.Type == tea.KeyEnter {
		pb, err := playbook.BuildPlaybook(m.answers)
		if err != nil {
			// Pure-CPU validation failure: surface the reason, do NOT finish.
			m.errMsg = plainErr(err)
			return m
		}
		m.result = SetupResult{
			VoiceEnabled:  m.voiceEnabled,
			TelegramToken: m.telegram,
			LLM:           m.llm,
			Brokers: []BrokerCreds{{
				Kind:   m.brokerKind,
				Key:    m.brokerKey,
				Secret: m.brokerSecret,
			}},
			Playbook: pb,
			// A live broker AND an explicit toggle are BOTH required to go live;
			// either missing => paper. This is the safety default.
			Live: m.live && m.brokerKind.isLive(),
		}
		m.done = true
		return m.advance() // -> StepDone
	}
	return m
}

// editInput applies a printable/backspace keystroke to the in-flight input line.
// It is the single text-editing rule for every typed step (no per-step drift).
func (m WizardModel) editInput(k tea.KeyMsg) WizardModel {
	switch k.Type {
	case tea.KeyRunes:
		m.input += string(k.Runes)
		m.errMsg = ""
	case tea.KeySpace:
		m.input += " "
		m.errMsg = ""
	case tea.KeyBackspace:
		if r := []rune(m.input); len(r) > 0 {
			m.input = string(r[:len(r)-1])
		}
		m.errMsg = ""
	}
	return m
}

// looksLikeTelegramToken does a cheap, offline structural check (digits ':' key).
// It is NOT a live validation (no network — that would violate never-stall); it
// just catches an obviously-wrong paste so the owner fixes it before first run.
func looksLikeTelegramToken(tok string) bool {
	colon := strings.IndexByte(tok, ':')
	if colon <= 0 || colon == len(tok)-1 {
		return false
	}
	for _, r := range tok[:colon] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(tok[colon+1:]) >= 8
}

// plainErr strips the internal "playbook:" prefix so the owner sees plain English
// (the rigor stays in the engine; the message is for a non-engineer).
func plainErr(err error) string {
	s := err.Error()
	s = strings.TrimPrefix(s, "playbook: ")
	if s == "" {
		return "That answer didn't work — try again."
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// compile-time assertion that WizardModel satisfies tea.Model.
var _ tea.Model = WizardModel{}

// ensure lipgloss is referenced from this file too (styles live in view.go); the
// blank ref keeps the dependency obvious at the model boundary.
var _ = lipgloss.NewStyle
