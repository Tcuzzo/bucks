package analyst

import (
	"strings"
)

// claimMarker is the line prefix a backend uses to emit a checkable claim that
// must be grounded. The convention keeps the contract explicit: a model line
// `CLAIM[key]: text` declares "this text depends on the fact `key`", and Ground
// verifies `key` against real evidence. A claim with no key (`CLAIM: text`) is a
// qualitative statement that still needs grounding-by-policy (it has no backing
// fact, so it cannot be presented as a hard number).
const claimMarker = "CLAIM"

// parseView turns raw backend text into a structured View. Parsing is
// conservative and never throws: an unrecognized or absent leaning maps to
// LeanNeutral (the honest default — we do not guess a direction the model did not
// state). LEAN/RATIONALE/CLAIM lines are recognized; everything else folds into
// the rationale so no model output is silently dropped.
//
// Recognized lines (case-insensitive keys):
//
//	LEAN: bullish|bearish|neutral
//	RATIONALE: <text>
//	CLAIM[<evidence-key>]: <text>     (a fact-bearing claim to ground)
//	CLAIM: <text>                     (a claim with no backing fact)
func parseView(symbol, raw string) View {
	v := View{Symbol: symbol, Lean: LeanNeutral}
	var rationale []string
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		key, rest, hasColon := splitKey(trimmed)
		if !hasColon {
			rationale = append(rationale, trimmed)
			continue
		}
		upper := strings.ToUpper(key)
		switch {
		case upper == "LEAN":
			v.Lean = parseLean(rest)
		case upper == "RATIONALE":
			if rest != "" {
				rationale = append(rationale, rest)
			}
		case strings.HasPrefix(upper, claimMarker):
			v.Claims = append(v.Claims, parseClaim(key, rest))
		default:
			// Unknown key — keep the whole line in the rationale, never drop it.
			rationale = append(rationale, trimmed)
		}
	}
	v.Rationale = strings.Join(rationale, " ")
	return v
}

// splitKey splits "KEY: rest" into (key, rest, true). If there is no colon it
// returns ("", line, false). Only the FIRST colon splits, so a rationale that
// itself contains a colon survives.
func splitKey(line string) (key, rest string, ok bool) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", line, false
	}
	return strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:]), true
}

// parseLean maps text to a Lean, defaulting to neutral on anything unrecognized.
func parseLean(s string) Lean {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "bullish", "bull", "long", "up":
		return LeanBullish
	case "bearish", "bear", "short", "down":
		return LeanBearish
	default:
		return LeanNeutral
	}
}

// parseClaim extracts an optional evidence key from a CLAIM line's key part. The
// key part is either "CLAIM" (no fact) or "CLAIM[evidence-key]" (fact-bearing).
func parseClaim(keyPart, text string) Claim {
	c := Claim{Text: text}
	open := strings.Index(keyPart, "[")
	closeIdx := strings.LastIndex(keyPart, "]")
	if open >= 0 && closeIdx > open {
		c.EvidenceKey = strings.TrimSpace(keyPart[open+1 : closeIdx])
	}
	return c
}
