package understanding

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"bucks/internal/analyst"
)

// --- test doubles -----------------------------------------------------------

// mockBackend is a deterministic analyst.Backend standing in for the OUTERMOST
// model transport (and nothing else): the grader, the rubric, the parser, and the
// grounding under test are all the real thing. It returns reply on Complete, or err
// when err != nil. calls counts invocations.
type mockBackend struct {
	name  string
	reply string
	err   error
	calls int
}

func (m *mockBackend) Name() string { return m.name }

func (m *mockBackend) Complete(_ context.Context, _ string) (string, error) {
	m.calls++
	if m.err != nil {
		return "", m.err
	}
	return m.reply, nil
}

// candidateWithBug is the CANDIDATE under interrogation. Its stated intent claims
// the key is validated; the code plainly does not validate it. The token
// "loadKey" is a real symbol in the code, so a failure citing it MUST ground;
// "encryptAtRest" appears NOWHERE, so a failure citing it must NOT ground.
func candidateWithBug() Candidate {
	return Candidate{
		Name:   "keystore.go",
		Intent: "loadKey must reject an empty key and never return a zero-value secret.",
		Code: `func loadKey(env string) string {
	return os.Getenv(env)
}`,
	}
}

// gradeJSON renders a model reply. verdict is the verdict the MODEL tries to
// stamp — the organ must ignore it and compute its own from the dimension scores.
func gradeJSON(spec, arch, types, test, sec int, verdict, cite string) string {
	return fmt.Sprintf(`{
  "verdict": %q,
  "dimensions": {
    "spec_adherence": %d,
    "architectural_fit": %d,
    "type_safety": %d,
    "testability": %d,
    "security": %d
  },
  "failures": [
    {"dimension": "spec_adherence", "detail": "no empty-key rejection", "cite": %q}
  ],
  "recovery_actions": ["reject an empty env value and return an error"],
  "confidence": 0.9
}`, verdict, spec, arch, types, test, sec, cite)
}

// --- PROOF 1: the organ REFUTES, and cannot be talked into approving ---------

// TestGrade_RefutesFalseClaimAndCannotBeTalkedIntoApproval is the HONESTY proof.
// An organ that can only confirm is an echo, and an organ that can only reject is a
// broken clock — so this drives the SAME organ with two transports and requires the
// verdicts to diverge. It also hands the refuting reply a "verdict":"APPROVED"
// stamp: the organ must IGNORE the model's verdict and compute REJECT from the
// dimension evidence itself.
func TestGrade_RefutesFalseClaimAndCannotBeTalkedIntoApproval(t *testing.T) {
	ctx := context.Background()
	c := candidateWithBug()

	// (a) The model refutes the candidate's claim — while stamping APPROVED.
	refuting := &mockBackend{name: "refuter", reply: gradeJSON(0, 2, 2, 1, 0, "APPROVED", "loadKey")}
	got, err := Grade(ctx, []analyst.Backend{refuting}, c)
	if err != nil {
		t.Fatalf("Grade (refuting): %v", err)
	}
	if got.Verdict != VerdictReject {
		t.Errorf("refuted candidate verdict = %q, want %q (the organ must not take the model's APPROVED stamp)", got.Verdict, VerdictReject)
	}
	if got.Passed() {
		t.Error("refuted candidate Passed() = true, want false")
	}
	if got.Total != 25 {
		t.Errorf("Total = %d, want 25 ((0+2+2+1+0)/20*100)", got.Total)
	}
	if len(got.Failures) != 1 || !strings.Contains(got.Failures[0].Detail, "empty-key") {
		t.Errorf("Failures = %+v, want the named refutation", got.Failures)
	}
	if !got.Failures[0].Grounded {
		t.Error("failure citing the real symbol \"loadKey\" must ground against the candidate")
	}
	if len(got.RecoveryActions) == 0 {
		t.Error("a refuting grade must carry recovery actions")
	}

	// (b) The SAME organ, a clean transport — it must be able to APPROVE, or it is a
	// broken clock rather than a grader.
	clean := &mockBackend{name: "clean", reply: gradeJSON(4, 4, 4, 4, 4, "REJECT", "loadKey")}
	ok, err := Grade(ctx, []analyst.Backend{clean}, c)
	if err != nil {
		t.Fatalf("Grade (clean): %v", err)
	}
	if ok.Verdict != VerdictApproved {
		t.Errorf("clean candidate verdict = %q, want %q (the organ must not take the model's REJECT stamp either)", ok.Verdict, VerdictApproved)
	}
	if !ok.Passed() {
		t.Error("clean candidate Passed() = false, want true")
	}
	if ok.Total != 100 {
		t.Errorf("Total = %d, want 100", ok.Total)
	}
}

// TestGrade_HallucinatedFailureDoesNotGround proves the organ applies BUCKS's own
// GHOST discipline to ITSELF: a failure citing a symbol that appears nowhere in the
// candidate is a FABRICATED failure and is labeled ungrounded, never relayed as
// established fact. Without this the organ built to catch false claims would make
// them.
func TestGrade_HallucinatedFailureDoesNotGround(t *testing.T) {
	c := candidateWithBug()
	// "encryptAtRest" is not in the candidate source at all.
	b := &mockBackend{name: "hallucinator", reply: gradeJSON(1, 2, 2, 1, 0, "REJECT", "encryptAtRest")}
	got, err := Grade(context.Background(), []analyst.Backend{b}, c)
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if len(got.Failures) != 1 {
		t.Fatalf("Failures = %+v, want 1", got.Failures)
	}
	if got.Failures[0].Grounded {
		t.Error("failure citing \"encryptAtRest\" (absent from the candidate) must NOT ground")
	}
	if len(got.UngroundedFailures()) != 1 {
		t.Errorf("UngroundedFailures() = %+v, want the fabricated failure surfaced", got.UngroundedFailures())
	}
}

// --- PROOF 2: model-unavailable is NEVER a pass ------------------------------

// TestGrade_NoBackendIsNeverAPass is the anti-silent-gate proof — the exact bug
// A sibling project shipped this for months: a "graceful non-blocking degrade" that still reported
// passed=true. "I could not check" must be IMPOSSIBLE to mistake for "it passed".
func TestGrade_NoBackendIsNeverAPass(t *testing.T) {
	got, err := Grade(context.Background(), nil, candidateWithBug())
	if !errors.Is(err, ErrNoBackend) {
		t.Errorf("Grade with no backend err = %v, want ErrNoBackend (loud, not swallowed)", err)
	}
	if got.Passed() {
		t.Fatal("no-backend Passed() = true — this is the silent gate; it must be false")
	}
	if got.Status != StatusUnavailable {
		t.Errorf("Status = %q, want %q", got.Status, StatusUnavailable)
	}
	if got.Verdict != VerdictUnavailable {
		t.Errorf("Verdict = %q, want %q — it must never read as APPROVED", got.Verdict, VerdictUnavailable)
	}
	if got.Verdict == VerdictApproved {
		t.Error("no-backend verdict must never be APPROVED")
	}
	if strings.TrimSpace(got.Guidance) == "" {
		t.Error("no-backend Guidance is empty — the owner gets no actionable setup step")
	}
	for _, want := range []string{"BUCKS_CHAT_PROVIDER", "BUCKS_CHAT_KEY", "build.nvidia.com"} {
		if !strings.Contains(got.Guidance, want) {
			t.Errorf("Guidance missing %q; got:\n%s", want, got.Guidance)
		}
	}
}

// TestZeroAssessmentIsNotAPass proves the ZERO VALUE of an Assessment cannot be
// mistaken for approval — a caller that forgets to check the error still holds a
// non-passing result.
func TestZeroAssessmentIsNotAPass(t *testing.T) {
	var a Assessment
	if a.Passed() {
		t.Error("zero-value Assessment.Passed() = true, want false")
	}
	if a.Verdict == VerdictApproved {
		t.Error("zero-value Assessment must not carry the APPROVED verdict")
	}
}

// TestGrade_AllBackendsFailedIsNeverAPass proves a transport failure surfaces
// LOUDLY and lands on UNAVAILABLE — never a fabricated neutral score reported as a
// pass (the starved-grading bug's tail).
func TestGrade_AllBackendsFailedIsNeverAPass(t *testing.T) {
	b := &mockBackend{name: "dead", err: errors.New("http 503")}
	got, err := Grade(context.Background(), []analyst.Backend{b}, candidateWithBug())
	if err == nil {
		t.Fatal("all-backends-failed must return a loud error")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error = %v, want it to name the underlying failure", err)
	}
	if got.Passed() {
		t.Error("all-backends-failed Passed() = true, want false")
	}
	if got.Verdict != VerdictUnavailable {
		t.Errorf("Verdict = %q, want %q", got.Verdict, VerdictUnavailable)
	}
}

// TestGrade_UnparseableReplyIsNeverAFabricatedScore proves a model reply that is
// not a usable grade FAILS rather than degrading into a neutral/fabricated score —
// the precise shape of the starved-grading bug, where a starved model's prose was parsed into
// a made-up passing number.
func TestGrade_UnparseableReplyIsNeverAFabricatedScore(t *testing.T) {
	b := &mockBackend{name: "prose", reply: "I think this code looks pretty reasonable overall!"}
	got, err := Grade(context.Background(), []analyst.Backend{b}, candidateWithBug())
	if err == nil {
		t.Fatal("an unparseable grade must return a loud error, not a neutral score")
	}
	if got.Passed() {
		t.Error("unparseable grade Passed() = true, want false")
	}
	if got.Total != 0 || got.Status != StatusUnavailable {
		t.Errorf("unparseable grade = {Total:%d Status:%q}, want a non-scored UNAVAILABLE result", got.Total, got.Status)
	}
}

// TestGrade_OutOfRangeScoreIsRejected proves the parser is STRICT: a dimension
// score outside 0..4 is an error, never clamped into a plausible-looking grade.
func TestGrade_OutOfRangeScoreIsRejected(t *testing.T) {
	b := &mockBackend{name: "liar", reply: gradeJSON(9, 9, 9, 9, 9, "APPROVED", "loadKey")}
	got, err := Grade(context.Background(), []analyst.Backend{b}, candidateWithBug())
	if err == nil {
		t.Fatal("an out-of-range dimension score must error, not be clamped")
	}
	if got.Passed() {
		t.Error("out-of-range grade Passed() = true, want false")
	}
}

// TestGrade_MissingDimensionIsRejected proves an incomplete grade is an error —
// the organ never fills a missing dimension with an invented score.
func TestGrade_MissingDimensionIsRejected(t *testing.T) {
	b := &mockBackend{name: "partial", reply: `{"dimensions":{"spec_adherence":4},"confidence":0.5}`}
	got, err := Grade(context.Background(), []analyst.Backend{b}, candidateWithBug())
	if err == nil {
		t.Fatal("a grade missing dimensions must error, not be back-filled")
	}
	// The reply carries only spec_adherence, so the FIRST absent dimension in the
	// canonical order is architectural_fit — the error must name it, not shrug.
	if !strings.Contains(err.Error(), "missing dimension") ||
		!strings.Contains(err.Error(), string(DimArchitecturalFit)) {
		t.Errorf("error = %v, want it to name the missing dimension %q", err, DimArchitecturalFit)
	}
	if got.Passed() {
		t.Error("incomplete grade Passed() = true, want false")
	}
}

// --- the deterministic rubric ------------------------------------------------

// TestVerdictBoundaries pins the verdict thresholds exactly at their edges, so a
// drift in either direction fails.
func TestVerdictBoundaries(t *testing.T) {
	cases := []struct {
		total int
		want  Verdict
	}{
		{100, VerdictApproved},
		{80, VerdictApproved},
		{79, VerdictRevise},
		{60, VerdictRevise},
		{59, VerdictReject},
		{0, VerdictReject},
	}
	for _, c := range cases {
		if got := VerdictFor(c.total); got != c.want {
			t.Errorf("VerdictFor(%d) = %q, want %q", c.total, got, c.want)
		}
	}
}

// TestTotalIsDeterministicAndScaled proves the aggregation is a pure 0..20 -> 0..100
// scale over the five dimensions, and that it is stable across repeated calls.
func TestTotalIsDeterministicAndScaled(t *testing.T) {
	scores := map[Dimension]int{
		DimSpecAdherence:    4,
		DimArchitecturalFit: 3,
		DimTypeSafety:       4,
		DimTestability:      2,
		DimSecurity:         3,
	}
	first, err := Total(scores)
	if err != nil {
		t.Fatalf("Total: %v", err)
	}
	if first != 80 { // (4+3+4+2+3)=16 -> 16/20*100
		t.Errorf("Total = %d, want 80", first)
	}
	for i := 0; i < 5; i++ {
		again, err := Total(scores)
		if err != nil || again != first {
			t.Fatalf("Total is not deterministic: %d/%v vs %d", again, err, first)
		}
	}
}

// TestGradingTokenBudgetFloor pins the documented floor: a sibling project's grounding organ
// was starved at 400 tokens and fabricated a passing score. The floor must stay at
// or above 2000.
func TestGradingTokenBudgetFloor(t *testing.T) {
	if MinGradingTokens < 2000 {
		t.Errorf("MinGradingTokens = %d, want >= 2000 (a reasoning model spends its budget thinking before it emits the payload)", MinGradingTokens)
	}
}

// TestPromptCarriesIntentCandidateAndRefutationDuty proves the organ interrogates
// the candidate against the ORIGINAL INTENT and explicitly invites refutation — an
// organ that only asks "is this good?" is an echo by construction.
func TestPromptCarriesIntentCandidateAndRefutationDuty(t *testing.T) {
	c := candidateWithBug()
	var seen string
	b := &captureBackend{reply: gradeJSON(4, 4, 4, 4, 4, "APPROVED", "loadKey"), seen: &seen}
	if _, err := Grade(context.Background(), []analyst.Backend{b}, c); err != nil {
		t.Fatalf("Grade: %v", err)
	}
	for _, want := range []string{c.Intent, c.Code, c.Name} {
		if !strings.Contains(seen, want) {
			t.Errorf("grading prompt missing %q; prompt:\n%s", want, seen)
		}
	}
	// The refutation DUTY must be stated, and the model must be told its own verdict
	// stamp carries no weight — otherwise the organ is asking for an echo.
	lower := strings.ToLower(seen)
	for _, want := range []string{"refute", "cite", "rubric"} {
		if !strings.Contains(lower, want) {
			t.Errorf("grading prompt missing %q; prompt:\n%s", want, seen)
		}
	}
	for _, d := range Dimensions {
		if !strings.Contains(seen, string(d)) {
			t.Errorf("grading prompt missing dimension %q", d)
		}
	}
}

// captureBackend records the prompt it was handed so a test can assert on the CALL
// CONTRACT rather than on a canned verdict.
type captureBackend struct {
	reply string
	seen  *string
}

func (c *captureBackend) Name() string { return "capture" }

func (c *captureBackend) Complete(_ context.Context, prompt string) (string, error) {
	*c.seen = prompt
	return c.reply, nil
}

// --- failover: every downgrade is recorded, never silent ---------------------

// TestGrade_RecordsFailoverToSecondBackend proves the organ inherits the analyst's
// no-silent-downgrade discipline: when the primary fails, the fallback's grade is
// returned WITH the downgrade recorded.
func TestGrade_RecordsFailoverToSecondBackend(t *testing.T) {
	dead := &mockBackend{name: "primary", err: errors.New("http 429")}
	alive := &mockBackend{name: "fallback", reply: gradeJSON(4, 4, 4, 4, 4, "APPROVED", "loadKey")}
	got, err := Grade(context.Background(), []analyst.Backend{dead, alive}, candidateWithBug())
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if got.Backend != "fallback" {
		t.Errorf("Backend = %q, want fallback", got.Backend)
	}
	if !got.Downgraded() || len(got.Failovers) != 1 {
		t.Errorf("Failovers = %+v, want the downgrade recorded (never silent)", got.Failovers)
	}
	if got.Failovers[0].From != "primary" {
		t.Errorf("Failovers[0].From = %q, want primary", got.Failovers[0].From)
	}
}
