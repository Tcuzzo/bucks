package analyst

import "strings"

// tokenize splits text into comparison tokens on whitespace and punctuation,
// while keeping a decimal point that sits BETWEEN two digits as part of its number
// (so "2.31" is one token, never "2" + "31"). Every other run of letters/digits is
// its own token; all separators (spaces, commas, colons, parens, a trailing
// sentence period, etc.) are dropped. This is what makes grounding a WORD/TOKEN
// match: evidence "1" cannot match the token "100", and "2" cannot match "2.31".
func tokenize(text string) []string {
	var tokens []string
	var cur strings.Builder
	runes := []rune(text)
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for i, r := range runes {
		switch {
		case isTokenRune(r):
			cur.WriteRune(r)
		case r == '.' && betweenDigits(runes, i):
			// A decimal point flanked by digits stays inside the number token.
			cur.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return tokens
}

// isTokenRune reports whether r is a letter or digit (the body of a token).
func isTokenRune(r rune) bool {
	return (r >= '0' && r <= '9') ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z')
}

// betweenDigits reports whether the rune at index i has a digit immediately
// before AND after it (so a decimal point inside a number is preserved, but a
// sentence-ending period is a separator).
func betweenDigits(runes []rune, i int) bool {
	if i <= 0 || i+1 >= len(runes) {
		return false
	}
	prev, next := runes[i-1], runes[i+1]
	return prev >= '0' && prev <= '9' && next >= '0' && next <= '9'
}

// tokensContainSubsequence reports whether the consecutive token sequence `want`
// appears as a whole-token run anywhere in `have`. An empty want never matches.
// This requires the evidence value to appear as a complete token (or complete run
// of tokens) in the claim text — not as a substring of a larger token.
func tokensContainSubsequence(have, want []string) bool {
	if len(want) == 0 || len(want) > len(have) {
		return false
	}
	for start := 0; start+len(want) <= len(have); start++ {
		match := true
		for j := range want {
			if have[start+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// GHOST grounding — BUCKS's honesty layer (spec §4.5: "no fabricated edge, no
// fake backtests"). A View coming back from a language model may carry claims: a
// cited number, a backtest stat, a claimed edge. None of those may be presented
// to the owner as FACT unless they are backed by REAL evidence the system
// actually has. Ground enforces that:
//
//   - Each Claim that declares an EvidenceKey is checked against the supplied
//     evidence map. It is marked Verified ONLY when the key resolves to a real
//     fact AND the claim's text is consistent with that fact (the fact's value
//     appears in the claim text). Otherwise it stays Unverified.
//   - Each Claim with NO EvidenceKey carries no checkable fact, so it can never be
//     "verified as a number" — it is always left Unverified. A qualitative read
//     is fine as opinion, but it is not promoted to fact.
//
// Ground never deletes a claim (the owner still sees what the model said) — it
// LABELS provenance, so unsupported numbers are surfaced as "unverified", never
// silently as truth. The View is marked Grounded so a consumer can refuse to
// present an ungrounded View's claims as fact.
//
// Evidence is a map of fact-key -> the real, system-computed value (e.g.
// {"atr14": "2.31", "expectancy": "0.18"}). The caller supplies ONLY facts it
// actually ran/computed — that is what makes the grounding real.

// Ground checks every claim in the view against the supplied evidence and returns
// a grounded copy. The input View is not mutated. After Ground, view.Grounded is
// true and each Claim.Verified reflects whether it was backed by real evidence.
func Ground(view View, evidence map[string]string) View {
	grounded := view
	// Copy claims so the original View is untouched (no shared backing array).
	claims := make([]Claim, len(view.Claims))
	copy(claims, view.Claims)
	for i := range claims {
		claims[i].Verified = claimSupported(claims[i], evidence)
	}
	grounded.Claims = claims
	grounded.Grounded = true
	return grounded
}

// claimSupported reports whether a single claim is backed by real evidence. A
// claim with no EvidenceKey is never supported (no checkable fact). A claim with
// an EvidenceKey is supported only when the key exists in evidence AND the
// evidence value appears as a WHOLE TOKEN (or whole consecutive token run) in the
// claim text — so a model that cites the RIGHT key but the WRONG number is NOT
// verified (it cannot launder a fabricated figure through a real key), and a
// fragment like "1" can never verify the larger number "100". This is a
// word/token-boundary match, not a substring match: substring matching let
// evidence "1" falsely verify the claim "RSI is 100".
func claimSupported(c Claim, evidence map[string]string) bool {
	if c.EvidenceKey == "" {
		return false
	}
	val, ok := evidence[c.EvidenceKey]
	if !ok {
		return false
	}
	// The real value's token sequence must appear as a whole-token run in the
	// claim text — guards against citing a real key but stating a different
	// (fabricated) number, and against a digit fragment matching a larger number.
	return EvidenceSupports(c.Text, val)
}

// EvidenceSupports reports whether value appears in text as a WHOLE TOKEN (or a
// whole consecutive run of tokens) — the token-boundary match that makes BUCKS's
// grounding real rather than a substring guess: evidence "1" can never verify the
// claim "RSI is 100", and "2" can never verify "2.31". An empty (or
// whitespace-only) value never supports anything.
//
// This is the single grounding primitive behind claimSupported. It is exported so
// any surface that must check "did the model cite something that actually EXISTS in
// the source it was handed?" reuses the ONE honesty engine instead of writing a
// second, subtly different matcher.
func EvidenceSupports(text, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	return tokensContainSubsequence(tokenize(text), tokenize(value))
}

// AllVerified reports whether a grounded view has no unverified claims. It is a
// convenience for a caller that will only auto-act on fully-grounded reads; it is
// false on an ungrounded view (claims have not been checked yet).
func AllVerified(view View) bool {
	if !view.Grounded {
		return false
	}
	for _, c := range view.Claims {
		if !c.Verified {
			return false
		}
	}
	return true
}
