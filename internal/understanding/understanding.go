// Package understanding is BUCKS's build-validation organ: it interrogates a
// CANDIDATE (a change, a file, a proposed implementation) against the ORIGINAL
// INTENT and returns EVIDENCE — per-dimension scores, named failures, recovery
// actions, and a confidence — rather than a thumbs-up.
//
// It is ADVISORY ONLY. It is not a gate, not an approval path, and not blocking:
// nothing in BUCKS asks it for permission, and it is deliberately kept OUT of the
// money path entirely (no order, no risk check, and no execution decision consults
// it — it grades BUILDS, not trades). It exists so a human reading its output
// learns something true about their change.
//
// THE HONESTY PROPERTY IS THE WHOLE POINT: the organ must be able to REFUTE, not
// merely confirm. An organ that can only confirm is an echo. Three things make the
// refutation real, and each is enforced by construction:
//
//   - THE MODEL DOES NOT GET A VOTE ON THE VERDICT. It supplies per-dimension
//     evidence (0..4) and named failures; the verdict is computed HERE by a pure,
//     deterministic rubric (Total + VerdictFor). A model reply that stamps
//     "verdict": "APPROVED" over failing dimension scores is ignored — the rubric
//     reads the evidence, not the stamp.
//   - A FABRICATED FAILURE IS LABELED, NOT RELAYED. Each failure must CITE text
//     from the candidate it was handed. The citation is checked against the real
//     candidate source with analyst.EvidenceSupports — the same whole-token GHOST
//     matcher the trading surfaces ground their numbers with. A failure citing
//     something that appears nowhere in the candidate comes back Grounded=false, so
//     the organ built to catch false claims never quietly makes one.
//   - "I COULD NOT CHECK" IS NEVER "IT PASSED". With no model configured, with
//     every backend failing, or with a reply that does not parse into a real grade,
//     the result is StatusUnavailable / VerdictUnavailable with Passed()==false and
//     a loud error — never a neutral score dressed up as approval. Laundering a
//     failed check into a pass is a silent gate, and it is the exact bug this organ
//     exists to refuse.
//
// The organ reuses BUCKS's OWN model seam verbatim: an analyst.Backend is the thin
// Complete(ctx, prompt) contract that already serves the analyst, chat, research,
// and summary surfaces, so every provider (NVIDIA NIM free Nemotron, Groq,
// Cerebras, OpenRouter, Ollama, OAuth-GPT, local codex) works here with no new HTTP
// code and no new dependency. It inherits the analyst's no-silent-downgrade
// discipline: every failover is RECORDED in the Assessment, never a quiet swap.
//
// Nothing here makes a network call in the default test suite — the Backend is
// driven by test doubles and httptest servers.
package understanding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"bucks/internal/analyst"
)

// MinGradingTokens is the completion budget a grading call MUST request. It is not
// arbitrary: BUCKS's free default brain is a REASONING model, and such a model
// spends its budget THINKING before it emits a single byte of payload. Budget it
// for the answer alone and `content` comes back EMPTY while the reasoning chain
// fills the response — the failure that silently degraded a sibling project's
// grader into fabricating passing scores for months.
//
// A grading reply carries five dimension scores, named failures with citations, and
// recovery actions, so it needs real room on top of the model's chain. Callers
// building a backend for this organ pass analyst.WithMaxTokens(MinGradingTokens).
const MinGradingTokens = 2000

// MaxDimensionScore is the top score for a single dimension. Five dimensions at
// 0..4 give a 0..20 raw score, scaled to the 0..100 total.
const MaxDimensionScore = 4

// Verdict thresholds — the documented rubric, applied deterministically by
// VerdictFor.
const (
	// ApprovedThreshold is the inclusive total at/above which a candidate is APPROVED.
	ApprovedThreshold = 80
	// ReviseThreshold is the inclusive total at/above which a candidate is REVISE.
	ReviseThreshold = 60
)

// ErrNoBackend is returned by Grade when no model is configured. It is a LOUD,
// checkable sentinel so a caller can print setup guidance and carry on (the organ
// is advisory — an absent model is never fatal), while a caller that ignores the
// error still receives an Assessment whose Passed() is false. Both paths are safe;
// neither can read as approval.
var ErrNoBackend = errors.New("understanding: no model backend configured")

// Dimension is one axis of the grade.
type Dimension string

// The five dimensions the organ grades, each 0..MaxDimensionScore.
const (
	// DimSpecAdherence — does the candidate do what the INTENT actually asked?
	DimSpecAdherence Dimension = "spec_adherence"
	// DimArchitecturalFit — does it fit the surrounding design, or fight it?
	DimArchitecturalFit Dimension = "architectural_fit"
	// DimTypeSafety — are the types honest, total, and hard to misuse?
	DimTypeSafety Dimension = "type_safety"
	// DimTestability — can its behavior be proven, or only asserted?
	DimTestability Dimension = "testability"
	// DimSecurity — what does it expose, trust, or leak?
	DimSecurity Dimension = "security"
)

// Dimensions is the canonical, ordered dimension set. Order is stable so prompts
// and printed output never shuffle between runs.
var Dimensions = []Dimension{
	DimSpecAdherence,
	DimArchitecturalFit,
	DimTypeSafety,
	DimTestability,
	DimSecurity,
}

// Verdict is the organ's advisory conclusion, computed from the dimension evidence
// by VerdictFor — never taken from the model's own say-so.
type Verdict string

const (
	// VerdictApproved — total >= ApprovedThreshold.
	VerdictApproved Verdict = "APPROVED"
	// VerdictRevise — ReviseThreshold <= total < ApprovedThreshold.
	VerdictRevise Verdict = "REVISE"
	// VerdictReject — total < ReviseThreshold.
	VerdictReject Verdict = "REJECT"
	// VerdictUnavailable — the candidate was NOT graded (no model, every backend
	// failed, or an unusable reply). It is a distinct verdict precisely so it can
	// never be mistaken for a pass.
	VerdictUnavailable Verdict = "UNAVAILABLE"
)

// Status separates "I graded this" from "I could not check". The zero value is
// neither, which is why the zero Assessment can never read as a pass.
type Status string

const (
	// StatusGraded — a model returned a usable grade and the rubric scored it.
	StatusGraded Status = "graded"
	// StatusUnavailable — no grade was produced. Never an approval.
	StatusUnavailable Status = "unavailable"
)

// Candidate is the thing under interrogation, together with the intent it is
// supposed to satisfy. Intent is the ORIGINAL ASK in the author's own words — the
// organ grades against it, not against its own taste.
type Candidate struct {
	// Name identifies the candidate (e.g. a file path) for the report.
	Name string
	// Intent is what the change was SUPPOSED to do. Grading without it would be
	// taste, not validation.
	Intent string
	// Code is the candidate source the model reads and every citation is checked
	// against.
	Code string
}

// Failure is one named defect the model reported. Cite is the text it copied from
// the candidate to justify the claim; Grounded records whether that citation
// actually appears in the candidate source.
type Failure struct {
	// Dimension is the axis this failure belongs to.
	Dimension Dimension
	// Detail is the plain-language defect.
	Detail string
	// Cite is the exact text from the candidate the model pointed at.
	Cite string
	// Grounded is true only when Cite appears as a whole-token run in the
	// candidate's real source. False means the model cited something that is NOT in
	// the code it was handed — a fabricated failure, surfaced as such rather than
	// relayed as fact.
	Grounded bool
}

// Assessment is the organ's evidence. There is no bare boolean anywhere in it:
// Passed() is derived, so a caller cannot be handed a `true` by accident.
type Assessment struct {
	// Candidate is the name of what was graded.
	Candidate string
	// Status is whether a grade happened at all.
	Status Status
	// Verdict is the rubric's advisory conclusion.
	Verdict Verdict
	// Total is the 0..100 score (0 when Status is StatusUnavailable — an ungraded
	// candidate has no score, not a neutral one).
	Total int
	// Scores is the per-dimension evidence, 0..MaxDimensionScore.
	Scores map[Dimension]int
	// Failures are the named defects, each with its citation grounded or not.
	Failures []Failure
	// RecoveryActions are the concrete next steps the model proposed.
	RecoveryActions []string
	// Confidence is the model's self-reported confidence, 0..1. It is reported, not
	// trusted: it never affects the verdict.
	Confidence float64
	// Backend is the model that produced the grade (the surviving backend after any
	// failover).
	Backend string
	// Failovers records every downgrade on the way to Backend — visible, never
	// silent (the analyst's discipline, inherited).
	Failovers []analyst.Failover
	// Guidance is plain-English setup help, present only when Status is
	// StatusUnavailable.
	Guidance string
}

// Passed reports whether the candidate was actually GRADED and actually APPROVED.
// It is a method over both fields on purpose: an ungraded candidate can never
// report true, and the ZERO Assessment (Status "") is not a pass. This is the
// anti-silent-gate property — "I could not check" cannot become "it passed".
func (a Assessment) Passed() bool {
	return a.Status == StatusGraded && a.Verdict == VerdictApproved
}

// Downgraded reports whether the grade was produced after at least one failover.
func (a Assessment) Downgraded() bool { return len(a.Failovers) > 0 }

// UngroundedFailures returns the failures whose citation does NOT appear in the
// candidate. A non-empty result means the model named a defect it could not point
// at — treat those as unverified, exactly as BUCKS treats an unbacked number.
func (a Assessment) UngroundedFailures() []Failure {
	var out []Failure
	for _, f := range a.Failures {
		if !f.Grounded {
			out = append(out, f)
		}
	}
	return out
}

// Rationale renders the assessment's evidence as one plain-language line. It is a
// convenience for logging/printing; it invents nothing that is not already in the
// Assessment.
func (a Assessment) Rationale() string {
	if a.Status != StatusGraded {
		return strings.TrimSpace(a.Guidance)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %d/100", a.Verdict, a.Total)
	for _, f := range a.Failures {
		fmt.Fprintf(&b, "; %s: %s", f.Dimension, f.Detail)
	}
	return b.String()
}

// Total scales the per-dimension evidence to 0..100 deterministically: the five
// dimensions sum to 0..20, times five. It is a pure function — identical scores
// always give an identical total, with no model in the loop.
//
// It VALIDATES rather than repairs: a missing dimension or a score outside
// 0..MaxDimensionScore is an error, never a clamp or a back-filled default. A
// grader that quietly invents a plausible number for evidence it does not have is
// the exact failure this organ exists to refuse.
func Total(scores map[Dimension]int) (int, error) {
	sum := 0
	for _, d := range Dimensions {
		v, ok := scores[d]
		if !ok {
			return 0, fmt.Errorf("understanding: grade is missing dimension %q", d)
		}
		if v < 0 || v > MaxDimensionScore {
			return 0, fmt.Errorf("understanding: dimension %q scored %d, outside 0..%d", d, v, MaxDimensionScore)
		}
		sum += v
	}
	return sum * 100 / (len(Dimensions) * MaxDimensionScore), nil
}

// VerdictFor maps a 0..100 total to the advisory verdict by the documented
// thresholds: APPROVED >= 80, REVISE 60..79, REJECT < 60. Pure and deterministic.
func VerdictFor(total int) Verdict {
	switch {
	case total >= ApprovedThreshold:
		return VerdictApproved
	case total >= ReviseThreshold:
		return VerdictRevise
	default:
		return VerdictReject
	}
}

// SetupGuidance is the plain-English "no brain configured yet" help, naming the
// FREE path first (a no-credit-card NVIDIA key) — the same voice and the same env
// seam the chat/summary/research surfaces use, so a user learns one setup, not four.
func SetupGuidance() string {
	return "No LLM backend configured, so nothing was checked — this is NOT a pass.\n" +
		"FREE option — get a no-credit-card key at build.nvidia.com (~2 min), then:\n" +
		"  BUCKS_CHAT_PROVIDER=nemotron BUCKS_CHAT_KEY=nvapi-... bucks understand <file> \"<what it should do>\"\n" +
		"Or point at any OpenAI/Ollama-compatible endpoint with BUCKS_CHAT_BASEURL (and optionally BUCKS_CHAT_KEY, BUCKS_CHAT_MODEL)."
}

// unavailable builds the honest "I could not check" Assessment: an explicit
// UNAVAILABLE verdict, no score, Passed()==false, and actionable guidance.
func unavailable(candidate, guidance string, failovers []analyst.Failover) Assessment {
	return Assessment{
		Candidate: candidate,
		Status:    StatusUnavailable,
		Verdict:   VerdictUnavailable,
		Guidance:  guidance,
		Failovers: failovers,
	}
}

// modelGrade is the strict wire shape of a model's reply. Note what is ABSENT: the
// model's own "verdict" is deliberately NOT decoded. It may write one; this organ
// does not read it. The verdict is the rubric's to compute from the evidence below,
// which is what makes the organ impossible to talk into an approval.
type modelGrade struct {
	Dimensions map[string]int `json:"dimensions"`
	Failures   []struct {
		Dimension string `json:"dimension"`
		Detail    string `json:"detail"`
		Cite      string `json:"cite"`
	} `json:"failures"`
	RecoveryActions []string `json:"recovery_actions"`
	Confidence      float64  `json:"confidence"`
}

// Grade interrogates the candidate against its intent over an ORDERED list of
// backends (primary first) and returns the evidence.
//
// It is ADVISORY: it blocks nothing and gates nothing. It is also HONEST in the
// only way that matters — every path that did not actually produce a grade returns
// StatusUnavailable with Passed()==false AND a loud error:
//
//   - no backends            -> ErrNoBackend + setup guidance
//   - a backend errors       -> the failover is RECORDED and the next backend tried
//   - an unusable reply      -> the failover is RECORDED and the next backend tried
//   - every backend exhausted-> a joined error naming each real failure
//
// A parse failure is NEVER softened into a neutral score, and the model's own
// verdict stamp is never read. On success the verdict is computed here, by the
// deterministic rubric, from the model's dimension evidence.
func Grade(ctx context.Context, backends []analyst.Backend, c Candidate) (Assessment, error) {
	if len(backends) == 0 {
		return unavailable(c.Name, SetupGuidance(), nil), ErrNoBackend
	}

	prompt := BuildPrompt(c)
	var failovers []analyst.Failover
	var errs []error
	for _, b := range backends {
		raw, err := b.Complete(ctx, prompt)
		if err != nil {
			failovers = append(failovers, analyst.Failover{From: b.Name(), Err: err.Error()})
			errs = append(errs, fmt.Errorf("backend %s: %w", b.Name(), err))
			continue
		}
		a, err := assessmentFrom(raw, c)
		if err != nil {
			// An unusable grade is a FAILED backend, recorded like any other
			// downgrade — never a neutral score standing in for a real answer.
			failovers = append(failovers, analyst.Failover{From: b.Name(), Err: err.Error()})
			errs = append(errs, fmt.Errorf("backend %s: %w", b.Name(), err))
			continue
		}
		a.Backend = b.Name()
		a.Failovers = failovers
		return a, nil
	}
	return unavailable(c.Name, SetupGuidance(), failovers),
		fmt.Errorf("understanding: all %d backend(s) failed to grade %q: %w",
			len(backends), c.Name, errors.Join(errs...))
}

// assessmentFrom parses one model reply into a scored Assessment, or errors. It
// applies the rubric (Total/VerdictFor) to the model's evidence and grounds every
// citation against the candidate's REAL source.
func assessmentFrom(raw string, c Candidate) (Assessment, error) {
	mg, err := parseGrade(raw)
	if err != nil {
		return Assessment{}, err
	}
	scores := make(map[Dimension]int, len(Dimensions))
	for _, d := range Dimensions {
		v, ok := mg.Dimensions[string(d)]
		if !ok {
			return Assessment{}, fmt.Errorf("understanding: grade is missing dimension %q", d)
		}
		scores[d] = v
	}
	total, err := Total(scores)
	if err != nil {
		return Assessment{}, err
	}
	failures := make([]Failure, 0, len(mg.Failures))
	for _, f := range mg.Failures {
		failures = append(failures, Failure{
			Dimension: Dimension(strings.TrimSpace(f.Dimension)),
			Detail:    strings.TrimSpace(f.Detail),
			Cite:      strings.TrimSpace(f.Cite),
			// GHOST grounding, reused verbatim from the trading surfaces: the
			// citation must actually appear in the code the model was handed.
			Grounded: analyst.EvidenceSupports(c.Code, f.Cite),
		})
	}
	return Assessment{
		Candidate:       c.Name,
		Status:          StatusGraded,
		Verdict:         VerdictFor(total),
		Total:           total,
		Scores:          scores,
		Failures:        failures,
		RecoveryActions: mg.RecoveryActions,
		Confidence:      mg.Confidence,
	}, nil
}

// parseGrade decodes the model's JSON payload. Models routinely wrap JSON in prose
// or a ```json fence, so the first balanced-looking object is extracted before
// decoding — but that is the ONLY tolerance: if no object is present, or it does not
// decode, or it carries no dimensions, this errors LOUDLY. There is no neutral
// fallback score, because a fabricated score reported as a real one is precisely the
// defect this organ exists to catch.
func parseGrade(raw string) (modelGrade, error) {
	obj := extractJSONObject(raw)
	if obj == "" {
		return modelGrade{}, fmt.Errorf("understanding: model reply contains no JSON grade: %s", snippet(raw))
	}
	var mg modelGrade
	if err := json.Unmarshal([]byte(obj), &mg); err != nil {
		return modelGrade{}, fmt.Errorf("understanding: model reply is not a decodable grade: %w", err)
	}
	if len(mg.Dimensions) == 0 {
		return modelGrade{}, fmt.Errorf("understanding: model reply carries no dimension scores: %s", snippet(raw))
	}
	return mg, nil
}

// extractJSONObject returns the outermost {...} span of s, or "" when there is
// none. It is a span extractor, not a validator — json.Unmarshal is the judge.
func extractJSONObject(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return ""
	}
	return s[start : end+1]
}

// snippet returns a short, single-line excerpt of an unusable reply so the error
// names what actually came back instead of hiding it.
func snippet(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// BuildPrompt composes the grading prompt from the candidate + its intent. It is
// deterministic given identical inputs, and it is exported so a caller can see
// EXACTLY what the model is asked — the interrogation is not a black box.
//
// The prompt's job is to make refutation the DUTY, not a permitted option: it asks
// for defects and citations, tells the model its verdict stamp will be ignored (the
// rubric owns the conclusion), and requires every failure to quote real text from
// the candidate — which is then checked, so a fabricated citation is caught rather
// than believed.
func BuildPrompt(c Candidate) string {
	var b strings.Builder
	b.WriteString("You are a rigorous code reviewer. Your duty is to REFUTE, not to agree.\n")
	b.WriteString("Interrogate the CANDIDATE against the INTENT it was supposed to satisfy and report EVIDENCE.\n")
	b.WriteString("Do not be agreeable: a review that only confirms is worthless. If the candidate does not do what the intent says, say so and prove it.\n\n")
	fmt.Fprintf(&b, "INTENT (what this was supposed to do):\n%s\n\n", strings.TrimSpace(c.Intent))
	fmt.Fprintf(&b, "CANDIDATE (%s):\n%s\n\n", c.Name, c.Code)
	b.WriteString("Score EACH dimension 0-4 (0 = absent/broken, 4 = fully satisfied):\n")
	b.WriteString("  spec_adherence     — does it do what the INTENT actually asked?\n")
	b.WriteString("  architectural_fit  — does it fit the surrounding design, or fight it?\n")
	b.WriteString("  type_safety        — are the types honest, total, and hard to misuse?\n")
	b.WriteString("  testability        — can its behavior be proven, or only asserted?\n")
	b.WriteString("  security           — what does it expose, trust, or leak?\n\n")
	b.WriteString("Every failure MUST carry a \"cite\": text copied EXACTLY from the CANDIDATE above.\n")
	b.WriteString("A citation that does not appear in the candidate is checked and marked fabricated, so do not invent one.\n")
	b.WriteString("The final verdict is computed from your dimension scores by a fixed rubric — writing a verdict yourself has no effect.\n\n")
	b.WriteString("Reply with JSON ONLY, in exactly this shape:\n")
	b.WriteString(`{"dimensions":{"spec_adherence":0,"architectural_fit":0,"type_safety":0,"testability":0,"security":0},` +
		`"failures":[{"dimension":"spec_adherence","detail":"what is wrong","cite":"exact text from the candidate"}],` +
		`"recovery_actions":["the concrete fix"],"confidence":0.0}` + "\n")
	return b.String()
}
