package summary

import (
	"strings"

	"bucks/internal/analyst"
)

// accountGround finds the ACCOUNT-FACT figures in a plain-English summary and
// grounds them against the REAL account facts, REUSING analyst.Ground — BUCKS's
// ONE honesty engine. This is the SAME approach the chat surface uses
// (internal/chat/ground.go): we do not write a third grounding implementation, we
// extract the numbers the model presented as the owner's account state and feed
// them through analyst.Ground so a true figure verifies and a fabricated one is
// flagged unverified (never presented as truth).
//
// PRECISION: not every number in the prose is an account claim. A list marker
// ("1.", "2."), a general rule ("risk 2%"), a textbook figure, or a hypothetical is
// ordinary language — flagging those as "unverified account figures" is noise. So a
// number becomes a grounded claim ONLY when it sits in an ACCOUNT-FACT context:
//
//   - it MATCHES a supplied real fact value (the model is restating a real number),
//     OR
//   - an account-fact cue (built-in cue language, or one of the supplied fact keys)
//     appears within accountCueWindow words of it.
//
// A number in neither context is left alone. Once a number IS an account claim, the
// contract holds via analyst.Ground: keyed to a matching fact => VERIFIED (the value
// must appear as a WHOLE TOKEN — the right number for a key stays honest); matching
// NO fact => UNVERIFIED (a fabricated figure is flagged, never presented as fact).
func accountGround(text string, facts map[string]string) []analyst.Claim {
	// Reverse index: numeric-core value -> fact key, so a number in the prose can be
	// matched to the fact it (claims to) restate. The Report values are already exact
	// Decimal strings; numericCore strips sign/currency so "+125.50" matches "125.50".
	valToKey := map[string]string{}
	for _, k := range sortedKeys(facts) {
		core := numericCore(facts[k])
		if core == "" {
			continue
		}
		if _, seen := valToKey[core]; !seen {
			valToKey[core] = k // first key wins, deterministic via sorted keys
		}
	}

	tokens := accountFactTokens(text, sortedKeys(facts), valToKey)
	if len(tokens) == 0 {
		return nil
	}

	var claims []analyst.Claim
	seen := map[string]bool{}
	for _, tok := range tokens {
		if seen[tok] {
			continue
		}
		seen[tok] = true
		key := valToKey[tok] // "" when this number matches no supplied fact
		claims = append(claims, analyst.Claim{
			Text:        "account figure stated in summary: " + tok,
			EvidenceKey: key,
		})
	}

	// Reuse the single honesty engine. Evidence values are normalized to numeric cores
	// so analyst.Ground's whole-token match lines up with the extracted tokens.
	view := analyst.View{Claims: claims}
	grounded := analyst.Ground(view, normalizedEvidence(facts))
	return grounded.Claims
}

// accountCues marks a number as a statement about the owner's REAL ACCOUNT STATE,
// as opposed to a general rule, an example, a list marker, or a hypothetical.
// Detection is proximity-based (a cue within accountCueWindow words). The supplied
// fact keys are added at call time so a model that echoes "realized_pnl"/"equity"
// is recognized too. Matched case-insensitively; multi-word cues are substrings.
var accountCues = []string{
	"your balance", "your account", "your p&l", "your pnl", "your p & l",
	"you're up", "youre up", "you are up", "you're down", "youre down", "you are down",
	"your position", "your positions", "you hold", "you have $", "you've got $",
	"your cash", "buying power", "your equity", "your drawdown", "your balance is",
	"your gain", "your loss", "your profit", "in the account", "account balance",
	"account is", "p&l is", "pnl is", "balance is", "you're sitting", "youre sitting",
	"you made", "you lost", "currently up", "currently down", "your buying power",
	"where you stand", "your unrealized", "your realized", "equity is", "equity of",
}

// accountCueWindow is how many words a number may sit from an account cue and still
// be treated as part of that account claim. A small window keeps a cue in one
// sentence from claiming a number in a different, unrelated sentence.
const accountCueWindow = 6

// accountFactTokens returns the numeric cores in the text presented as the owner's
// ACCOUNT STATE: a number that restates a supplied fact value (so a correct
// restatement verifies and a stale one is caught), or a number with an account cue
// within accountCueWindow words. Numbers with no nearby cue and no fact match are
// ordinary language and are NOT returned (never flagged).
func accountFactTokens(text string, factKeys []string, valToKey map[string]string) []string {
	lower := strings.ToLower(text)

	cues := make([]string, 0, len(accountCues)+len(factKeys))
	cues = append(cues, accountCues...)
	for _, k := range factKeys {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		cues = append(cues, k)
		if spaced := strings.ReplaceAll(k, "_", " "); spaced != k {
			cues = append(cues, spaced)
		}
	}

	cuePos := cuePositions(lower, cues)

	words := splitWords(text)
	var out []string
	for i, w := range words {
		core := numericCore(w.text)
		if core == "" {
			continue
		}
		if _, isFactValue := valToKey[core]; isFactValue {
			out = append(out, core) // restates a real number — always ground it
			continue
		}
		if cueNearWord(words, i, cuePos, accountCueWindow) {
			out = append(out, core) // account-context number with no backing fact
		}
	}
	return out
}

// word is one whitespace-delimited word with its rune span, so a numeric word's
// distance to a cue is measured both in words (the window) and resolved against the
// cue's character position.
type word struct {
	text  string
	start int
	end   int
}

func splitWords(text string) []word {
	var out []word
	runes := []rune(text)
	i := 0
	for i < len(runes) {
		for i < len(runes) && isSpace(runes[i]) {
			i++
		}
		if i >= len(runes) {
			break
		}
		start := i
		for i < len(runes) && !isSpace(runes[i]) {
			i++
		}
		out = append(out, word{text: string(runes[start:i]), start: start, end: i})
	}
	return out
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v'
}

// cuePositions returns the rune offsets of every account-cue occurrence in the
// lower-cased text. Multi-word cues match as substrings.
func cuePositions(lower string, cues []string) []int {
	runeStart := byteToRuneOffsets(lower)
	var positions []int
	for _, cue := range cues {
		if cue == "" {
			continue
		}
		from := 0
		for {
			idx := strings.Index(lower[from:], cue)
			if idx < 0 {
				break
			}
			abs := from + idx
			positions = append(positions, runeStart[abs])
			from = abs + len(cue)
			if from >= len(lower) {
				break
			}
		}
	}
	return positions
}

func byteToRuneOffsets(s string) []int {
	out := make([]int, len(s)+1)
	ri := 0
	for b := range s {
		out[b] = ri
		ri++
	}
	out[len(s)] = ri
	return out
}

// cueNearWord reports whether any account cue lies within `window` WORDS of the
// word at index i.
func cueNearWord(words []word, i int, cuePos []int, window int) bool {
	if len(cuePos) == 0 {
		return false
	}
	for _, pos := range cuePos {
		cueWord := wordIndexAt(words, pos)
		if cueWord < 0 {
			continue
		}
		d := i - cueWord
		if d < 0 {
			d = -d
		}
		if d <= window {
			return true
		}
	}
	return false
}

func wordIndexAt(words []word, pos int) int {
	for j, w := range words {
		if pos < w.end {
			return j
		}
	}
	return len(words) - 1
}

// numericCore strips sign, currency, percent, grouping commas, and a trailing dot
// from a figure and returns the bare number ("+$1,250.50" -> "1250.50", "58%" ->
// "58"). It returns "" when there is no real number (so a lone "$"/"+" is not a
// figure). A value must contain at least one digit to qualify.
func numericCore(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, ",", "")
	s = strings.TrimLeft(s, "+-$ ")
	s = strings.TrimRight(s, "%. ")
	if s == "" {
		return ""
	}
	hasDigit := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r == '.':
			// allowed inside a number
		default:
			return ""
		}
	}
	if !hasDigit {
		return ""
	}
	return s
}

// normalizedEvidence returns the facts with each numeric VALUE reduced to its
// numeric core, so analyst.Ground's whole-token match aligns with the tokens
// extracted from the prose. Non-numeric facts (mode=paper, halted=no) pass through.
func normalizedEvidence(facts map[string]string) map[string]string {
	if len(facts) == 0 {
		return nil
	}
	out := make(map[string]string, len(facts))
	for k, v := range facts {
		if core := numericCore(v); core != "" {
			out[k] = core
		} else {
			out[k] = v
		}
	}
	return out
}
