package tui

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// styleSet holds the precomputed lipgloss styles. lipgloss style application is a
// pure string transform (it wraps text in ANSI codes); it performs no I/O, so
// using it inside View never violates the never-stall invariant.
type styleSet struct {
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
		// BUCKS synthwave-neon arcade palette — cohesive with the green/gold wordmark up top:
		// neon-cyan section headers, neon-green "go", neon-magenta danger, dim-violet hints, on
		// the terminal's own dark background.
		header: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#2DE2FF")),  // neon cyan
		prompt: lipgloss.NewStyle().Foreground(lipgloss.Color("#E6E6F0")),             // bright
		hint:   lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("#8A86C8")), // dim violet
		errBox: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF3366")),  // neon magenta-red
		good:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#39FF14")),  // neon green
		bad:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF3366")),  // neon magenta-red
		dim:    lipgloss.NewStyle().Faint(true),
	}
}

// BuckBanner is the BUCKS block wordmark (spec §1: clean bold GOLD block wordmark,
// no animal; $ money accent on the tagline). It is the PLAIN (uncolored) art so it
// renders on Linux AND Windows terminals alike, and so README/non-TTY surfaces and
// tests can match exact chars. Six block-letter lines spell BUCKS in box-drawing
// blocks; the seventh is a tagline whose leading "$" is the money accent. The block
// char "█" and the literal "$" appear so the brand mark is assertable in tests.
const BuckBanner = `
 ██████╗ ██╗   ██╗ ██████╗██╗  ██╗███████╗
 ██╔══██╗██║   ██║██╔════╝██║ ██╔╝██╔════╝
 ██████╔╝██║   ██║██║     █████╔╝ ███████╗
 ██╔══██╗██║   ██║██║     ██╔═██╗ ╚════██║
 ██████╔╝╚██████╔╝╚██████╗██║  ██╗███████║
 ╚═════╝  ╚═════╝  ╚═════╝╚═╝  ╚═╝╚══════╝
   $  the 8-point buck — a trader, not an assistant
`

// bannerRenderer is a lipgloss renderer pinned to the 256-color profile. The wordmark
// is the brand mark and must ALWAYS show in color — so we do not rely on stdout TTY
// auto-detection (which strips color when stdout is a pipe/file, e.g. under tests or
// `bucks logo | less`). Pinning ANSI256 here matches the 256-palette color codes
// below and keeps the colored output deterministic everywhere, without mutating the
// global default renderer the rest of the wizard uses for its own TTY-aware styles.
var bannerRenderer = newBannerRenderer()

func newBannerRenderer() *lipgloss.Renderer {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.ANSI256)
	return r
}

// banner colors — retro-arcade neon in the operator's palette: GREEN primary, GOLD
// accent, BLACK background (the terminal's own black). The six block-letter wordmark
// lines get a PER-LINE vertical GREEN gradient (bright neon/lawn green at the top
// fading to gold on the bottom/shadow row — "green and gold"), mirroring HydraAgent's
// vertical-gradient-on-a-figlet arcade look. The tagline is gold with a bright-green
// "$" money accent. Precomputed once (pure string ops, no I/O).
var (
	// bannerGradient: top figlet line -> bottom figlet line. Six neon-green stops
	// fading to a gold accent on the last (shadow) row. Each is its OWN bold style so
	// a whole figlet line is colored in one Render (no nesting). Truecolor hex; the
	// pinned ANSI256 renderer downsamples it deterministically (so it colors when piped).
	bannerGradient = []lipgloss.Style{
		bannerRenderer.NewStyle().Bold(true).Foreground(lipgloss.Color("#7CFC00")), // lawn green
		bannerRenderer.NewStyle().Bold(true).Foreground(lipgloss.Color("#39FF14")), // neon green
		bannerRenderer.NewStyle().Bold(true).Foreground(lipgloss.Color("#1AE676")),
		bannerRenderer.NewStyle().Bold(true).Foreground(lipgloss.Color("#00E676")),
		bannerRenderer.NewStyle().Bold(true).Foreground(lipgloss.Color("#00C853")), // deep green
		bannerRenderer.NewStyle().Bold(true).Foreground(lipgloss.Color("#D4A24E")), // gold accent
	}
	// bannerWord is the canonical wordmark style (the first/top gradient stop). Tests
	// derive the expected wordmark escape from it, so the assertion can't drift.
	bannerWord = bannerGradient[0]
	// bannerGold: tagline body in gold. bannerDollar: bright neon-green "$" accent.
	bannerGold   = bannerRenderer.NewStyle().Foreground(lipgloss.Color("#D4A24E")) // gold (tagline body)
	bannerDollar = bannerRenderer.NewStyle().Foreground(lipgloss.Color("#39FF14")) // neon green $
)

// RenderBanner returns the fully-colored BUCKS wordmark for a TTY in the retro-arcade
// neon palette: the six block-letter figlet lines get a PER-LINE vertical GREEN
// gradient (bright neon green at the top fading to a gold accent on the bottom row),
// and the tagline is gold with its "$" money accent in bright green. Each figlet line
// is colored as ONE styled unit in its gradient stop (one Render per line — like
// HydraAgent appends each line with one color; no nested Render). The tagline is split
// on "$" — the non-"$" segments gold and the "$" green, joined — so the green "$" is
// never produced by nesting one Render inside another (which corrupts ANSI). It is a
// pure string transform (no I/O), safe to call every frame.
func RenderBanner() string {
	lines := strings.Split(BuckBanner, "\n")
	figletIdx := 0 // counts the figlet art rows, top -> bottom, for the gradient stop
	for i, line := range lines {
		// Each figlet ART row gets its own vertical gradient stop. "Art row" = a line of
		// the block wordmark — block "█" rows AND the bottom box-drawing shadow row
		// ("╚═════╝...") which carries no "█" but IS the last figlet row, so it takes the
		// final (gold) stop. isFigletArt distinguishes those from the tagline + blanks.
		// Clamp to the last stop so extra figlet rows (if the art grows) still color.
		if isFigletArt(line) {
			stop := figletIdx
			if stop >= len(bannerGradient) {
				stop = len(bannerGradient) - 1
			}
			lines[i] = bannerGradient[stop].Render(line)
			figletIdx++
			continue
		}
		lines[i] = colorDollarLine(line)
	}
	return strings.Join(lines, "\n")
}

// isFigletArt reports whether a banner line is a row of the BUCKS block wordmark (so it
// takes a green-gradient stop) as opposed to the tagline or a blank spacer. The figlet
// art is built only from full-block and box-drawing glyphs; the tagline is plain text
// with a "$". A line is art if it contains a block "█" OR is made of box-drawing pieces
// (the bottom shadow row "╚═════╝") and carries no "$".
func isFigletArt(line string) bool {
	if strings.ContainsAny(line, "$") {
		return false
	}
	if strings.Contains(line, "█") {
		return true
	}
	// Bottom shadow row: only box-drawing glyphs + spaces, and at least one of them.
	hasBox := false
	for _, r := range line {
		switch r {
		case ' ', '╗', '╔', '╝', '╚', '═', '║', '╠', '╣', '╦', '╩', '╬':
			if r != ' ' {
				hasBox = true
			}
		default:
			return false // any other glyph (plain text) => not pure art
		}
	}
	return hasBox
}

// colorDollarLine renders one banner line: gold for the text, neon green for each
// "$". It splits on "$" and renders the in-between gold segments and each "$" green
// separately, then concatenates — no nested Render, so the ANSI stays clean.
func colorDollarLine(line string) string {
	if !strings.Contains(line, "$") {
		return bannerGold.Render(line)
	}
	segs := strings.Split(line, "$")
	var b strings.Builder
	for i, seg := range segs {
		if seg != "" {
			b.WriteString(bannerGold.Render(seg))
		}
		if i < len(segs)-1 { // a "$" sat between this segment and the next
			b.WriteString(bannerDollar.Render("$"))
		}
	}
	return b.String()
}

// bannerView renders the COLORED BUCKS wordmark. Kept a method so the wizard header
// and the welcome screen share one brand mark (now with proper per-line color).
func (m WizardModel) bannerView() string {
	return RenderBanner()
}

// View implements tea.Model. It is a pure function of the model state to a string
// — no I/O, no blocking — so it is safe to call every frame and trivial to assert.
func (m WizardModel) View() string {
	var b strings.Builder

	// Header carries the brand mark + a clear locator on every screen. The six
	// real setup steps show "step K of 6"; the terminal StepDone is a completion
	// state, not a 7th step, so it shows a clean "complete" locator instead.
	var header string
	if m.step == StepDone {
		header = "$ BUCKS setup — complete"
	} else {
		header = fmt.Sprintf("$ BUCKS setup — step %d of %d: %s%s",
			m.stepNumber(), len(stepOrder)-1, m.step.String(), m.headerSubProgress())
	}
	b.WriteString(m.styles.header.Render(header))
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
		if m.llmKeyPhase {
			// Second StepLLM prompt: capture the API key for the key-based backend. It is
			// masked (mask) so the key is never shoulder-surfed off the screen.
			b.WriteString(m.styles.prompt.Render(m.llm.keyPrompt()))
			b.WriteString("\n")
			b.WriteString("  key > " + mask(m.input) + "\n")
			b.WriteString(m.styles.hint.Render("  type key   [enter] save   [esc] pick a different backend\n"))
		} else {
			b.WriteString(m.styles.prompt.Render("Which thinking backend should BUCKS use?"))
			b.WriteString("\n")
			b.WriteString(choiceLine("1", "ChatGPT login — no API key (needs the codex CLI)", m.llm == LLMOAuthGPT))
			b.WriteString(choiceLine("2", "Cloud API key", m.llm == LLMCloudKey))
			b.WriteString(choiceLine("3", "Both (ChatGPT primary + cloud-key fallback)", m.llm == LLMBoth))
			b.WriteString(choiceLine("4", "Free (NVIDIA Nemotron) — no paid key", m.llm == LLMNemotronFree))
			if m.llm == LLMOAuthGPT || m.llm == LLMBoth {
				b.WriteString(m.styles.hint.Render(
					"  ChatGPT login uses the free codex CLI you're signed into — no API key.\n" +
						"  No codex / no ChatGPT? Press [4] for the free brain (works for everyone).\n"))
			}
			if m.llm == LLMNemotronFree {
				b.WriteString(m.styles.hint.Render(
					"  Free brain — no paid key, no Ollama needed. Get a free nvapi-... key at\n" +
						"  build.nvidia.com (about 2 minutes, no credit card), then paste it next.\n"))
			}
			b.WriteString(m.styles.hint.Render("  [1/2/3/4] pick   [enter] confirm   [esc] back\n"))
		}
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

// headerSubProgress surfaces the sub-steps that live INSIDE a single Step so the "step K of
// 6" locator never hides them from the owner (the source of the "advertised 6 but really ~16
// screens" complaint): the 10-question intake shows its running question, and the two masked
// sub-prompts (the LLM API key and the broker API secret) are named. Pure string, no I/O;
// empty when the current screen is the step's first/only one.
func (m WizardModel) headerSubProgress() string {
	switch {
	case m.step == StepIntake && m.intakeIdx < len(m.intake.Questions):
		return fmt.Sprintf(" · question %d of %d", m.intakeIdx+1, len(m.intake.Questions))
	case m.step == StepLLM && m.llmKeyPhase:
		return " · your API key"
	case m.step == StepBroker && m.brokerSecretPhase:
		return " · your API secret"
	default:
		return ""
	}
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
