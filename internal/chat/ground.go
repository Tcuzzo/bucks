package chat

import (
	"sort"
	"strings"

	"bucks/internal/analyst"
)

// groundReply finds the ACCOUNT-FACT claims in a free-form chat reply and grounds
// them against the REAL account facts, REUSING analyst.Ground (BUCKS's one honesty
// engine — no second grounding implementation).
//
// PRECISION (the fix): not every number in a reply is an account claim. A list
// marker ("1, 2, 3"), a general rule ("risk 2%"), a textbook example ("1.5x ATR")
// and a hypothetical ("5 grand") are ordinary language, NOT a statement about the
// owner's real account — flagging them as "unverified account figures" is noise that
// makes BUCKS look broken. So a number becomes a grounded claim ONLY when it sits in
// an ACCOUNT-FACT context:
//
//   - it appears near account-fact CUE language (e.g. "you're up", "your P&L",
//     "your balance", "you hold") or near one of the supplied fact KEYS, OR
//   - it MATCHES a supplied fact value (the model is restating a real account number).
//
// A number in neither context is left alone — no claim, no flag.
//
// Once a number IS an account claim, the existing contract holds via analyst.Ground:
//
//   - if the number matches a supplied fact value, the claim is keyed to that fact and
//     grounds VERIFIED (the value must appear as a WHOLE TOKEN — the right number for a
//     key stays honest);
//   - if it matches NO supplied fact (a fabricated account figure, e.g. "you're up
//     +999.99" with no fact to back it), the claim has no matching key and analyst.Ground
//     leaves it UNVERIFIED — flagged, never presented as truth.
//
// We never delete a claim (the owner still sees what was said) — we LABEL provenance.
func groundReply(reply string, facts map[string]string) []analyst.Claim {
	// Build a reverse index value -> fact key so a number in the reply can be matched
	// to the fact it (claims to) come from. Normalize the value to its numeric core
	// so "+125.50" in the facts matches the "125.50" a model is likely to write.
	valToKey := map[string]string{}
	for _, k := range sortedFactKeys(facts) {
		core := numericCore(facts[k])
		if core == "" {
			continue
		}
		// First key wins for a given value (deterministic via sorted keys).
		if _, seen := valToKey[core]; !seen {
			valToKey[core] = k
		}
	}

	// Extract ONLY the numbers presented as the owner's account state — numbers in an
	// account-fact context (cue language / fact keys nearby) or that restate a fact
	// value. The supplied fact keys widen the cue set so a model that echoes "pnl" or
	// "buying_power" is recognized too.
	tokens := accountFactTokens(reply, sortedFactKeys(facts), valToKey)
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
			// The claim text carries the token so analyst.Ground's whole-token match
			// has the value present when (and only when) the key actually backs it.
			Text:        "account figure stated in reply: " + tok,
			EvidenceKey: key,
		})
	}

	// Reuse the single honesty engine. We pass the facts with their values normalized
	// to numeric cores so the whole-token match in analyst.Ground lines up with the
	// tokens we extracted from the reply (e.g. fact "+125.50" -> evidence "125.50").
	view := analyst.View{Claims: claims}
	grounded := analyst.Ground(view, normalizedEvidence(facts))
	return grounded.Claims
}

// accountCues are the phrases that mark a number as a statement about the OWNER'S
// REAL ACCOUNT STATE (balance, P&L, cash, buying power, a held position) — as opposed
// to a general rule, a textbook example, a hypothetical, or a list marker. Detection
// is proximity-based: a number is an account claim only when one of these cues sits
// within accountCueWindow tokens of it. The cues are matched case-insensitively and
// some are multi-word ("buying power"), so we scan a lower-cased, token-windowed view
// of the reply. The supplied FactProvider keys are added to this set at call time so
// a model echoing a real key ("pnl", "buying_power") is recognized too.
var accountCues = []string{
	"your balance", "your account", "your p&l", "your pnl", "your p & l",
	"you're up", "youre up", "you are up", "you're down", "youre down", "you are down",
	"your position", "your positions", "you hold", "you have $", "you've got $",
	"your cash", "buying power", "your equity", "your drawdown", "your balance is",
	"your gain", "your loss", "your profit", "in the account", "account balance",
	"account is", "p&l is", "pnl is", "balance is", "you're sitting", "youre sitting",
	"you made", "you lost", "currently up", "currently down", "your buying power",
}

// accountCueWindow is how many words a number may be from an account cue and still be
// treated as part of that account claim. A cue and its number normally share a clause
// ("your P&L is +125.50", "you're up 999.99 on the day"); a small window keeps a cue
// in one sentence from claiming a number in a different, unrelated sentence.
const accountCueWindow = 6

// accountFactTokens returns the numeric cores in the reply that are presented as the
// owner's ACCOUNT STATE. A number qualifies when EITHER (a) it restates a supplied
// fact value (valToKey has it — the model is echoing a real account number, so we must
// confirm it grounds), OR (b) an account cue (a built-in cue or one of the supplied
// fact keys) appears within accountCueWindow words of it. Numbers with no nearby cue
// and no fact match are ordinary language (list markers, rules, examples) and are NOT
// returned — they are never flagged.
func accountFactTokens(reply string, factKeys []string, valToKey map[string]string) []string {
	// Word-level scan so proximity is measured in words, and so a number token can be
	// located relative to cue words. We keep the original text for substring cue
	// matching (multi-word cues like "buying power") and a parallel word index that
	// maps each word to its rune offset for the proximity check.
	lower := strings.ToLower(reply)

	// Cue set: built-in account phrases + the live fact keys (normalized to spaces so
	// "buying_power" matches "buying power" too).
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

	// Find the rune positions of every account cue occurrence in the reply.
	cuePositions := cuePositions(lower, cues)

	// Walk the words; for each word that carries a numeric core, decide whether it is
	// in an account context (matches a fact value, or a cue is within the window).
	words := splitWords(reply)
	var out []string
	for i, w := range words {
		core := numericCore(w.text)
		if core == "" {
			continue
		}
		if _, isFactValue := valToKey[core]; isFactValue {
			// The model restated a real account number — always ground it (so a
			// correct restatement verifies and a stale one would be caught).
			out = append(out, core)
			continue
		}
		if cueNearWord(words, i, cuePositions, accountCueWindow) {
			// A number presented as the owner's account state but NOT backed by any
			// supplied fact — an account claim with no fact to verify it.
			out = append(out, core)
		}
		// else: ordinary language (list marker, rule, example, hypothetical) — skip.
	}
	return out
}

// word is one whitespace-delimited word of the reply with its rune-offset span, so a
// numeric word's distance to a cue can be measured both in words (window) and resolved
// against the cue's character position.
type word struct {
	text  string
	start int // rune index of the word's first rune in the original reply
	end   int // rune index one past the word's last rune
}

// splitWords breaks the reply into whitespace-delimited words, recording each word's
// rune span. Punctuation stays attached (numericCore strips it); this keeps the word
// index aligned with the character positions used for cue matching.
func splitWords(reply string) []word {
	var out []word
	runes := []rune(reply)
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

// cuePositions returns the rune offsets (start of match) of every account-cue
// occurrence within the lower-cased reply. Multi-word cues are matched as substrings.
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

// byteToRuneOffsets maps each byte index in s to its rune index, so a strings.Index
// byte offset can be expressed as a rune offset aligned with the word spans.
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

// cueNearWord reports whether any account cue lies within `window` WORDS of the word
// at index i. Proximity is measured in words on the cue side too: we find the word
// whose span contains each cue's start offset, then check the word-index distance.
func cueNearWord(words []word, i int, cuePositions []int, window int) bool {
	if len(cuePositions) == 0 {
		return false
	}
	for _, pos := range cuePositions {
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

// wordIndexAt returns the index of the word whose rune span contains (or starts at)
// the given rune offset, or the nearest following word. -1 if none.
func wordIndexAt(words []word, pos int) int {
	for j, w := range words {
		if pos < w.end {
			return j
		}
	}
	return len(words) - 1
}

// numericCore strips sign, currency, percent, grouping commas and a trailing dot
// from a figure and returns the bare number (e.g. "+$1,250.50" -> "1250.50",
// "58%" -> "58", "2.31." -> "2.31"). It returns "" when there is no real number
// (so a lone "$" or "+" is not treated as a figure). A value must contain at least
// one digit to qualify.
func numericCore(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, ",", "") // drop thousands separators
	// Trim leading sign/currency glue.
	s = strings.TrimLeft(s, "+-$ ")
	// Trim trailing percent / punctuation.
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
			// A stray non-numeric char means this wasn't a clean figure.
			return ""
		}
	}
	if !hasDigit {
		return ""
	}
	return s
}

// normalizedEvidence returns the facts with each VALUE reduced to its numeric core
// (when it has one), so analyst.Ground's whole-token match aligns with the tokens
// extracted from the reply. Non-numeric facts (e.g. mode=paper) are passed through
// unchanged — they are context, not figure claims.
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

// sortedFactKeys / sortedKeys give a deterministic key order (used for the reverse
// index and the rendered facts block).
func sortedFactKeys(facts map[string]string) []string { return sortedKeys(facts) }

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
