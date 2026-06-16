package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// styleSet holds the precomputed lipgloss styles. lipgloss style application is a
// pure string transform (it wraps text in ANSI codes); it performs no I/O, so
// using it inside View never violates the never-stall invariant.
type styleSet struct {
	banner lipgloss.Style
	header lipgloss.Style
	prompt lipgloss.Style
	hint   lipgloss.Style
	errBox lipgloss.Style
	good   lipgloss.Style
	bad    lipgloss.Style
	dim    lipgloss.Style
}

func newStyles() styleSet {
	return styleSet{
		banner: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("178")), // gold $
		header: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("33")),  // blue
		prompt: lipgloss.NewStyle().Foreground(lipgloss.Color("252")),            // bright
		hint:   lipgloss.NewStyle().Faint(true),                                  // dim hint
		errBox: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196")), // red
		good:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("46")),  // green
		bad:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196")), // red
		dim:    lipgloss.NewStyle().Faint(true),
	}
}

// BuckBanner is the 8-point-buck-with-a-$ mascot (spec §1: 8-point-buck mascot, $
// motif). It is plain ASCII so it renders on Linux AND Windows terminals alike.
// The literal "$" and "BUCKS" appear so the brand mark is assertable in tests.
const BuckBanner = `
     \\   $   //
      \\__|__//
       (o   o)       B U C K S
        \\ ^ /        the 8-point-buck trading agent
        /| $ |\\       ___  $  ___
       /_|___|_\\
         //  \\\\
`

// bannerView renders the styled buck banner. Kept a method so the wizard header
// and the welcome screen share one mascot.
func (m WizardModel) bannerView() string {
	return m.styles.banner.Render(BuckBanner)
}

// View implements tea.Model. It is a pure function of the model state to a string
// — no I/O, no blocking — so it is safe to call every frame and trivial to assert.
func (m WizardModel) View() string {
	var b strings.Builder

	// Header carries the brand mark + a clear step locator on every screen.
	b.WriteString(m.styles.header.Render(
		fmt.Sprintf("$ BUCKS setup — step %d of %d: %s", m.stepNumber(), len(stepOrder)-1, m.step.String())))
	b.WriteString("\n\n")

	switch m.step {
	case StepWelcome:
		b.WriteString(m.bannerView())
		b.WriteString("\n")
		b.WriteString(m.styles.prompt.Render("Welcome — let's unpack BUCKS together."))
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("  Voice: %s\n", onOff(m.voiceEnabled)))
		b.WriteString(m.styles.hint.Render("  [v] toggle voice   [enter] begin   [ctrl+c] quit\n"))
	case StepTelegram:
		b.WriteString(m.styles.prompt.Render("Paste your Telegram bot token (its OWN bot — never shared):"))
		b.WriteString("\n  > " + mask(m.input) + "\n")
		b.WriteString(m.styles.hint.Render("  [enter] save   [esc] back\n"))
	case StepLLM:
		b.WriteString(m.styles.prompt.Render("Which thinking backend should BUCKS use?"))
		b.WriteString("\n")
		b.WriteString(choiceLine("1", "OAuth-GPT", m.llm == LLMOAuthGPT))
		b.WriteString(choiceLine("2", "Cloud API key", m.llm == LLMCloudKey))
		b.WriteString(choiceLine("3", "Both (primary + fallback)", m.llm == LLMBoth))
		b.WriteString(m.styles.hint.Render("  [1/2/3] pick   [enter] confirm   [esc] back\n"))
	case StepBroker:
		if m.brokerSecretPhase {
			b.WriteString(m.styles.prompt.Render("Now paste the API SECRET for " + string(m.brokerKind) + ":"))
			b.WriteString("\n")
			b.WriteString("  secret > " + mask(m.input) + "\n")
			b.WriteString(m.styles.hint.Render("  type secret   [enter] save   [esc] re-enter key\n"))
		} else {
			b.WriteString(m.styles.prompt.Render("Pick a broker, then paste its API key:"))
			b.WriteString("\n")
			b.WriteString(choiceLine("1", "Alpaca — PAPER (safe default)", m.brokerKind == BrokerAlpacaPaper))
			b.WriteString(choiceLine("2", "Alpaca — LIVE (real money)", m.brokerKind == BrokerAlpacaLive))
			b.WriteString(choiceLine("3", "Coinbase", m.brokerKind == BrokerCoinbase))
			b.WriteString(choiceLine("4", "Tradier", m.brokerKind == BrokerTradier))
			b.WriteString("  key > " + mask(m.input) + "\n")
			b.WriteString(m.styles.hint.Render("  [1-4] pick   type key   [enter] next   [esc] back\n"))
		}
	case StepIntake:
		b.WriteString(m.intakeView())
	case StepSafety:
		b.WriteString(m.safetyView())
	case StepDone:
		b.WriteString(m.styles.good.Render("Setup complete — BUCKS is unpacked. $"))
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("  Mode: %s\n", modeLabel(m.result.Live)))
	}

	if m.errMsg != "" {
		b.WriteString("\n")
		b.WriteString(m.styles.errBox.Render("  ! " + m.errMsg))
		b.WriteString("\n")
	}
	return b.String()
}

// intakeView renders the current intake question (the real DefaultIntake script).
func (m WizardModel) intakeView() string {
	var b strings.Builder
	if m.intakeIdx >= len(m.intake.Questions) {
		b.WriteString(m.styles.prompt.Render("All questions answered — press enter to continue."))
		b.WriteString("\n")
		return b.String()
	}
	q := m.intake.Questions[m.intakeIdx]
	b.WriteString(m.styles.prompt.Render(fmt.Sprintf(
		"Q%d/%d: %s", m.intakeIdx+1, len(m.intake.Questions), q.Prompt)))
	b.WriteString("\n")
	if len(q.Options) > 0 {
		b.WriteString(m.styles.hint.Render("  choices: " + strings.Join(q.Options, " | ") + "\n"))
	}
	if !q.Required {
		b.WriteString(m.styles.hint.Render("  (optional — press enter to skip)\n"))
	}
	b.WriteString("  > " + m.input + "\n")
	b.WriteString(m.styles.hint.Render("  [enter] answer   [esc] back\n"))
	return b.String()
}

// safetyView renders the final paper/live confirmation. Paper is shown in green as
// the safe default; live in red so the owner cannot miss that real money is armed.
func (m WizardModel) safetyView() string {
	var b strings.Builder
	b.WriteString(m.styles.prompt.Render("Final check — how should BUCKS trade?"))
	b.WriteString("\n")
	if m.live && m.brokerKind.isLive() {
		b.WriteString("  Mode: " + m.styles.bad.Render("LIVE — real money") + "\n")
	} else {
		b.WriteString("  Mode: " + m.styles.good.Render("PAPER — simulated (safe)") + "\n")
	}
	b.WriteString(fmt.Sprintf("  Broker: %s\n", m.brokerKind))
	if m.brokerKind.isLive() {
		b.WriteString(m.styles.hint.Render("  [l] toggle LIVE/paper   [enter] finish   [esc] back\n"))
	} else {
		b.WriteString(m.styles.hint.Render("  paper broker selected — live is off by default   [enter] finish   [esc] back\n"))
	}
	return b.String()
}

// stepNumber is the 1-based display index of the current step (StepDone shows the
// last number).
func (m WizardModel) stepNumber() int {
	for i, s := range stepOrder {
		if s == m.step {
			return i + 1
		}
	}
	return 1
}

// --- small pure render helpers (no I/O) ---

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func modeLabel(live bool) string {
	if live {
		return "LIVE (real money)"
	}
	return "PAPER (simulated)"
}

// choiceLine renders one selectable option with a marker for the current pick.
func choiceLine(key, label string, selected bool) string {
	marker := " "
	if selected {
		marker = "x"
	}
	return fmt.Sprintf("  [%s] %s) %s\n", marker, key, label)
}

// mask hides a secret-ish field as it is typed (shows length, not the value), so a
// token isn't shoulder-surfed off the screen. It is a pure string transform.
func mask(s string) string {
	if s == "" {
		return ""
	}
	return strings.Repeat("•", len([]rune(s)))
}
