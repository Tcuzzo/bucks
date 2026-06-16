package analyst

import "testing"

// rawWithClaims is a backend reply carrying two fact-bearing claims (one whose
// number matches real evidence, one whose number is fabricated) plus a no-key
// qualitative claim. It exercises every grounding branch.
const rawWithClaims = "" +
	"LEAN: bullish\n" +
	"RATIONALE: pullback buy in an uptrend\n" +
	"CLAIM[atr14]: the 14-period ATR is 2.31\n" + // matches evidence -> verified
	"CLAIM[expectancy]: a backtest shows expectancy of 0.42 per trade\n" + // wrong number -> unverified
	"CLAIM: this setup usually rips\n" // no key -> unverified (no checkable fact)

// TestGround_SupportedClaimVerified proves a claim whose evidence key resolves to
// a value that appears in the claim text grounds as verified (presentable as fact).
func TestGround_SupportedClaimVerified(t *testing.T) {
	v := parseView("AAPL", rawWithClaims)
	evidence := map[string]string{
		"atr14":      "2.31", // real, computed value
		"expectancy": "0.18", // the REAL backtest expectancy — NOT 0.42
	}
	g := Ground(v, evidence)
	if !g.Grounded {
		t.Fatal("grounded view must have Grounded=true")
	}
	atr := findClaim(t, g, "atr14")
	if !atr.Verified {
		t.Errorf("atr14 claim should be verified (2.31 matches evidence); got unverified")
	}
}

// TestGround_FabricatedNumberIsUnverified is the core honesty test: a claim that
// cites a REAL key but the WRONG number (a fabricated backtest stat) must NOT be
// verified — a fabricated edge can never launder itself through a real key.
func TestGround_FabricatedNumberIsUnverified(t *testing.T) {
	v := parseView("AAPL", rawWithClaims)
	evidence := map[string]string{"atr14": "2.31", "expectancy": "0.18"}
	g := Ground(v, evidence)

	exp := findClaim(t, g, "expectancy")
	if exp.Verified {
		t.Error("expectancy claim states 0.42 but real evidence is 0.18 — must be UNVERIFIED, not presented as fact")
	}
	// The unverified set surfaces it (so the owner sees "unverified", never fact).
	unv := g.UnverifiedClaims()
	if len(unv) == 0 {
		t.Fatal("expected unverified claims to be surfaced, got none")
	}
	foundExp := false
	for _, c := range unv {
		if c.EvidenceKey == "expectancy" {
			foundExp = true
		}
	}
	if !foundExp {
		t.Error("fabricated expectancy claim is not in the unverified set")
	}
}

// TestGround_NoKeyClaimNeverFact proves a qualitative claim with no evidence key
// is always left unverified — opinion is fine, but it is never promoted to fact.
func TestGround_NoKeyClaimNeverFact(t *testing.T) {
	v := parseView("AAPL", rawWithClaims)
	g := Ground(v, map[string]string{"atr14": "2.31"})
	for _, c := range g.Claims {
		if c.EvidenceKey == "" && c.Verified {
			t.Errorf("no-key claim %q must never be verified", c.Text)
		}
	}
}

// TestGround_MissingEvidenceIsUnverified proves a claim whose key is absent from
// the evidence map is unverified (we never had the fact, so it cannot be fact).
func TestGround_MissingEvidenceIsUnverified(t *testing.T) {
	v := parseView("AAPL", "LEAN: neutral\nCLAIM[sharpe]: the strategy sharpe is 1.9\n")
	g := Ground(v, map[string]string{}) // no evidence at all
	sharpe := findClaim(t, g, "sharpe")
	if sharpe.Verified {
		t.Error("a claim with no supporting evidence must be unverified")
	}
	if AllVerified(g) {
		t.Error("AllVerified must be false when an unsupported claim exists")
	}
}

// TestGround_CarriesProvenance proves a grounded view records per-claim provenance
// (verified vs unverified) AND that a fully-supported view reports AllVerified.
func TestGround_CarriesProvenance(t *testing.T) {
	v := parseView("AAPL", "LEAN: bullish\nCLAIM[atr14]: ATR is 2.31\nCLAIM[rsi]: RSI is 58\n")
	g := Ground(v, map[string]string{"atr14": "2.31", "rsi": "58"})
	if len(g.Claims) != 2 {
		t.Fatalf("expected 2 claims, got %d", len(g.Claims))
	}
	for _, c := range g.Claims {
		if !c.Verified {
			t.Errorf("claim %q (key %q) should be verified", c.Text, c.EvidenceKey)
		}
	}
	if !AllVerified(g) {
		t.Error("a fully-supported grounded view should be AllVerified")
	}
}

// TestGround_DoesNotMutateInput proves Ground returns a copy; the original View's
// claims are untouched (no shared backing array).
func TestGround_DoesNotMutateInput(t *testing.T) {
	v := parseView("AAPL", "CLAIM[atr14]: ATR is 2.31\n")
	_ = Ground(v, map[string]string{"atr14": "2.31"})
	if v.Grounded {
		t.Error("original view must not be marked Grounded by Ground")
	}
	if v.Claims[0].Verified {
		t.Error("original view's claim must not be mutated by Ground")
	}
}

// TestGround_TokenBoundary_NoSubstringLaunder is the BUG-3 honesty test: grounding
// is a WORD/TOKEN-boundary match, not a substring match. A fragment of a number can
// never verify a larger number, and the wrong number can never verify a claim.
//   - evidence "1" must NOT verify "RSI is 100" (100 contains "1" as a substring).
//   - evidence "2" must NOT verify "ATR is 2.31" (2.31 starts with "2").
//   - exact token "2.31" MUST verify "the ATR is 2.31".
func TestGround_TokenBoundary_NoSubstringLaunder(t *testing.T) {
	cases := []struct {
		name     string
		claim    string // CLAIM line text
		key      string
		value    string
		verified bool
	}{
		{"frag 1 cannot verify 100", "RSI is 100", "rsi", "1", false},
		{"frag 2 cannot verify 2.31", "ATR is 2.31", "atr14", "2", false},
		{"exact 2.31 verifies 2.31", "the ATR is 2.31", "atr14", "2.31", true},
		{"exact 100 verifies 100", "RSI is 100", "rsi", "100", true},
		{"exact 58 verifies RSI is 58", "RSI is 58", "rsi", "58", true},
		{"58 cannot verify 580", "level is 580", "lvl", "58", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Claim{Text: tc.claim, EvidenceKey: tc.key}
			got := claimSupported(c, map[string]string{tc.key: tc.value})
			if got != tc.verified {
				t.Errorf("claimSupported(%q, {%q:%q}) = %v, want %v",
					tc.claim, tc.key, tc.value, got, tc.verified)
			}
		})
	}
}

// TestGround_MultiTokenValueMatches proves a value made of several tokens grounds
// only when the whole consecutive token run appears in the claim text.
func TestGround_MultiTokenValueMatches(t *testing.T) {
	// A two-token value "20 day" appears verbatim in the claim -> verified.
	c := Claim{Text: "pulled back to the 20 day average", EvidenceKey: "ma"}
	if !claimSupported(c, map[string]string{"ma": "20 day"}) {
		t.Error(`"20 day" should verify a claim containing the run "20 day"`)
	}
	// The same tokens out of order / not consecutive must NOT verify.
	c2 := Claim{Text: "day 20 of the trade", EvidenceKey: "ma"}
	// "day 20 of" -> tokens day,20,of ; want 20,day -> not a consecutive run.
	if claimSupported(c2, map[string]string{"ma": "20 day"}) {
		t.Error(`"20 day" must NOT verify when the tokens are not a consecutive run`)
	}
}

// findClaim returns the claim with the given evidence key, failing the test if
// absent.
func findClaim(t *testing.T, v View, key string) Claim {
	t.Helper()
	for _, c := range v.Claims {
		if c.EvidenceKey == key {
			return c
		}
	}
	t.Fatalf("no claim with evidence key %q in view (claims=%+v)", key, v.Claims)
	return Claim{}
}
