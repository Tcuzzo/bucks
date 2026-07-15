package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"bucks/internal/analyst"
	"bucks/internal/orders"
	"bucks/internal/risk"
	"bucks/internal/secrets"
)

// summaryScriptBackend is a deterministic mock analyst.Backend for the summary CLI
// test: it returns a fixed reply so the command is exercised without any network.
type summaryScriptBackend struct {
	name       string
	reply      string
	seenPrompt *string
}

func (b *summaryScriptBackend) Name() string { return b.name }

func (b *summaryScriptBackend) Complete(_ context.Context, prompt string) (string, error) {
	if b.seenPrompt != nil {
		*b.seenPrompt = prompt
	}
	return b.reply, nil
}

// mockBackendsFactory builds an ordered backend list over the script backend — the
// injection seam that keeps the summary CLI test offline.
func mockBackendsFactory(reply string) backendsFactory {
	return func() ([]analyst.Backend, error) {
		return []analyst.Backend{&summaryScriptBackend{name: "mock", reply: reply}}, nil
	}
}

func captureBackendsFactory(reply string, seenPrompt *string) backendsFactory {
	return func() ([]analyst.Backend, error) {
		return []analyst.Backend{&summaryScriptBackend{name: "mock", reply: reply, seenPrompt: seenPrompt}}, nil
	}
}

func swapSummaryRuntime(
	out *bytes.Buffer,
	backends backendsFactory,
	configPath func() string,
	passphrase func() string,
	secretOptions func() []secrets.Option,
) func() {
	prevOut := summaryOutput
	prevBackends := summaryBackends
	prevConfigPath := summaryConfigPath
	prevPassphrase := summaryPassphrase
	prevSecretOptions := summarySecretOptions
	summaryOutput = out
	summaryBackends = backends
	summaryConfigPath = configPath
	summaryPassphrase = passphrase
	summarySecretOptions = secretOptions
	return func() {
		summaryOutput = prevOut
		summaryBackends = prevBackends
		summaryConfigPath = prevConfigPath
		summaryPassphrase = prevPassphrase
		summarySecretOptions = prevSecretOptions
	}
}

// TestRunSummary_DispatchesAndPrints proves `bucks summary` builds a summary from the
// demo snapshot and PRINTS it — the real entry point, driven with a mock backend
// (no network). The true demo P&L (125.50) grounds, so no unverified heads-up fires.
func TestRunSummary_DispatchesAndPrints(t *testing.T) {
	var out bytes.Buffer
	reply := "Here's where you stand today: your realized P&L is 125.50 on paper. Nothing is promised."
	if err := runSummary(&out, mockBackendsFactory(reply), demoSnapshot()); err != nil {
		t.Fatalf("runSummary: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Here's where you stand today") {
		t.Errorf("summary text not printed; output:\n%s", got)
	}
	if !strings.Contains(got, "125.50") {
		t.Errorf("summary should print the grounded P&L; output:\n%s", got)
	}
	// The real number grounds, so the unverified heads-up must NOT appear.
	if strings.Contains(got, "unverified") {
		t.Errorf("true figure grounded; no unverified note expected; output:\n%s", got)
	}
}

// TestRunSummary_FlagsFabricatedFigure proves the CLI surfaces an unbacked figure: a
// model that invents +999.99 triggers the "unverified" heads-up on the real entry point.
func TestRunSummary_FlagsFabricatedFigure(t *testing.T) {
	var out bytes.Buffer
	reply := "Here's where you stand today: your P&L is up +999.99 — amazing!"
	if err := runSummary(&out, mockBackendsFactory(reply), demoSnapshot()); err != nil {
		t.Fatalf("runSummary: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "unverified") {
		t.Errorf("fabricated figure should trigger the unverified note; output:\n%s", got)
	}
}

func TestRunSummaryFromConfigUsesSavedSetupNotDemoSnapshot(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "summary-config-pass"
	want := validSetupResult(t)
	want.Playbook.Capital = orders.MustParseDecimal("43210")
	if err := SaveSetup(want, configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("SaveSetup: %v", err)
	}

	var out bytes.Buffer
	var prompt string
	reply := "Your saved paper account is flat with equity 43210 and zero positions."
	err := runSummaryFromConfig(
		&out,
		captureBackendsFactory(reply, &prompt),
		configPath,
		pass,
		secrets.ForceFileBackend(),
	)
	if err != nil {
		t.Fatalf("runSummaryFromConfig: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "43210") {
		t.Fatalf("summary must use saved setup capital, got:\n%s", got)
	}
	if strings.Contains(got, "10125.50") || strings.Contains(got, "125.50") || strings.Contains(got, "AAPL") {
		t.Fatalf("summary leaked the old demo snapshot, got:\n%s", got)
	}
	if strings.Contains(got, "unverified") {
		t.Fatalf("saved setup figures should be grounded, got:\n%s", got)
	}
	if !strings.Contains(prompt, "equity = 43210") {
		t.Fatalf("summary prompt must be grounded on saved setup capital, got:\n%s", prompt)
	}
	for _, demo := range []string{"10125.50", "125.50", "AAPL"} {
		if strings.Contains(prompt, demo) {
			t.Fatalf("summary prompt leaked demo fact %q:\n%s", demo, prompt)
		}
	}
	if !strings.Contains(prompt, "OPEN POSITIONS: none") {
		t.Fatalf("summary prompt must show the saved setup is flat, got:\n%s", prompt)
	}
}

func TestRunSummaryDispatchUsesSavedSetupNotDemoSnapshot(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "summary-dispatch-pass"
	want := validSetupResult(t)
	want.Playbook.Capital = orders.MustParseDecimal("54321")
	if err := SaveSetup(want, configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("SaveSetup: %v", err)
	}

	var out bytes.Buffer
	var prompt string
	restore := swapSummaryRuntime(
		&out,
		captureBackendsFactory("Here's where you stand today: equity is 54321 and positions are flat.", &prompt),
		func() string { return configPath },
		func() string { return pass },
		func() []secrets.Option { return []secrets.Option{secrets.ForceFileBackend()} },
	)
	defer restore()

	if err := run([]string{"summary"}); err != nil {
		t.Fatalf("run(summary): %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "54321") {
		t.Fatalf("summary dispatch must print saved setup facts, got:\n%s", got)
	}
	if strings.Contains(got, "10125.50") || strings.Contains(got, "125.50") || strings.Contains(got, "AAPL") {
		t.Fatalf("summary dispatch leaked demo snapshot, got:\n%s", got)
	}
	if !strings.Contains(prompt, "equity = 54321") {
		t.Fatalf("summary dispatch prompt must use saved setup capital, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "10125.50") || strings.Contains(prompt, "125.50") || strings.Contains(prompt, "AAPL") {
		t.Fatalf("summary dispatch prompt leaked demo facts:\n%s", prompt)
	}
}

func TestRunSummaryDispatchHonorsConfigFlag(t *testing.T) {
	dir := t.TempDir()
	defaultConfig := filepath.Join(dir, "default.yaml")
	requestedConfig := filepath.Join(dir, "requested.yaml")
	const pass = "summary-config-flag-pass"
	defaultSetup := validSetupResult(t)
	defaultSetup.Playbook.Capital = orders.MustParseDecimal("11111")
	requestedSetup := validSetupResult(t)
	requestedSetup.Playbook.Capital = orders.MustParseDecimal("77777")
	if err := SaveSetup(defaultSetup, defaultConfig, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("SaveSetup default: %v", err)
	}
	if err := SaveSetup(requestedSetup, requestedConfig, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("SaveSetup requested: %v", err)
	}

	var out bytes.Buffer
	var prompt string
	restore := swapSummaryRuntime(
		&out,
		captureBackendsFactory("Here's where you stand today: equity is 77777.", &prompt),
		func() string { return defaultConfig },
		func() string { return pass },
		func() []secrets.Option { return []secrets.Option{secrets.ForceFileBackend()} },
	)
	defer restore()

	if err := run([]string{"summary", "--config", requestedConfig}); err != nil {
		t.Fatalf("run(summary --config): %v", err)
	}
	if !strings.Contains(prompt, "equity = 77777") {
		t.Fatalf("summary --config must use requested config, got prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "11111") {
		t.Fatalf("summary --config used the default config instead:\n%s", prompt)
	}
}

func TestRunSummaryDispatchPromptIncludesDurableHaltAndStandalonePaperMode(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "summary-dispatch-halt-pass"
	want := liveArmedSetupResult(t)
	if err := SaveSetup(want, configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("SaveSetup: %v", err)
	}
	ks, err := risk.Open(filepath.Join(dir, "killswitch.json"))
	if err != nil {
		t.Fatalf("open kill switch: %v", err)
	}
	if err := ks.Halt("operator /halt", risk.HaltManual); err != nil {
		t.Fatalf("halt: %v", err)
	}

	var out bytes.Buffer
	var prompt string
	restore := swapSummaryRuntime(
		&out,
		captureBackendsFactory("Here's where you stand today: trading is halted.", &prompt),
		func() string { return configPath },
		func() string { return pass },
		func() []secrets.Option { return []secrets.Option{secrets.ForceFileBackend()} },
	)
	defer restore()

	if err := run([]string{"summary"}); err != nil {
		t.Fatalf("run(summary): %v", err)
	}
	if !strings.Contains(prompt, "halted = yes") {
		t.Fatalf("summary dispatch prompt must include durable halted state, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "mode = paper") {
		t.Fatalf("standalone summary prompt must report safe paper mode, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "mode = live") {
		t.Fatalf("standalone summary prompt must not promote saved live arm to live mode:\n%s", prompt)
	}
}

func TestLoadSummarySnapshotReportsDurableHaltAndDoesNotPromoteSavedLiveArm(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "summary-halt-pass"
	want := liveArmedSetupResult(t)
	if err := SaveSetup(want, configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("SaveSetup: %v", err)
	}
	ks, err := risk.Open(filepath.Join(dir, "killswitch.json"))
	if err != nil {
		t.Fatalf("open kill switch: %v", err)
	}
	if err := ks.Halt("operator /halt", risk.HaltManual); err != nil {
		t.Fatalf("halt: %v", err)
	}

	snap, err := loadSummarySnapshot(configPath, pass, secrets.ForceFileBackend())
	if err != nil {
		t.Fatalf("loadSummarySnapshot: %v", err)
	}
	if !snap.halted {
		t.Fatal("summary snapshot must report the durable kill switch halt")
	}
	if snap.mode != "paper" {
		t.Fatalf("standalone summary must report paper mode, got %q", snap.mode)
	}
}

// TestRunSummary_NoBackendIsClearNotCrash proves the no-config path: with no backend
// (factory returns nil) the command prints a clear message naming the env vars and
// returns nil — it never crashes for lack of an LLM.
func TestRunSummary_NoBackendIsClearNotCrash(t *testing.T) {
	var out bytes.Buffer
	noBackend := func() ([]analyst.Backend, error) { return nil, nil }
	if err := runSummary(&out, noBackend, demoSnapshot()); err != nil {
		t.Fatalf("runSummary with no backend should not error, got: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "no LLM backend configured") {
		t.Errorf("no-backend message missing; output:\n%s", got)
	}
	if !strings.Contains(got, envChatBaseURL) {
		t.Errorf("no-backend message should name %s; output:\n%s", envChatBaseURL, got)
	}
}

// TestRunSummary_DispatchedFromRun proves the actual CLI dispatch reaches the summary
// path. With no chat env set, it prints the clear no-backend message and exits 0 —
// proving the entry point is wired without needing a live model.
func TestRunSummary_DispatchedFromRun(t *testing.T) {
	t.Setenv(envChatBaseURL, "")  // ensure no Ollama endpoint is configured in the test env
	t.Setenv(envChatProvider, "") // and no OpenAI-compatible provider, so the path stays offline
	var out bytes.Buffer
	restore := swapSummaryRuntime(
		&out,
		func() ([]analyst.Backend, error) { return nil, nil },
		defaultConfigPath,
		passphraseFromEnv,
		func() []secrets.Option { return nil },
	)
	defer restore()
	if err := run([]string{"summary"}); err != nil {
		t.Fatalf("`bucks summary` dispatch errored: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "no LLM backend configured") {
		t.Errorf("dispatch no-backend message missing; output:\n%s", got)
	}
	if !strings.Contains(got, envChatBaseURL) {
		t.Errorf("dispatch no-backend message should name %s; output:\n%s", envChatBaseURL, got)
	}
}
