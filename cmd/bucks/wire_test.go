package main

import (
	"path/filepath"
	"testing"

	"bucks/internal/secrets"
)

// TestPersistSetupWritesConfigAndRoundTrips is the end-to-end proof that was MISSING:
// a completed wizard result, when persisted via persistSetup, leaves a config file on
// disk (so configExists is true afterward — the wizard will NOT re-run) AND survives a
// LoadSetup round trip (broker, playbook, paper mode). This is the real fix for the
// "wizard restarts every launch" blocker: the wizard result is actually saved.
//
// HERMETIC: secrets.ForceFileBackend() keeps every secret on the temp file so the test
// never touches the developer's real OS keychain.
func TestPersistSetupWritesConfigAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "persist-setup-pass"

	want := validSetupResult(t)

	// BEFORE: no config -> the wizard would re-run forever.
	if configExists(configPath) {
		t.Fatal("precondition: config must not exist before persistSetup")
	}

	if err := persistSetup(want, configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("persistSetup: %v", err)
	}

	// AFTER: the config file EXISTS, so the next launch opens the dashboard, not the wizard.
	if !configExists(configPath) {
		t.Fatal("persistSetup did not leave a config file on disk (wizard would re-run)")
	}

	// And it round-trips: the saved setup loads back with the broker, playbook, paper mode.
	got, err := LoadSetup(configPath, pass, secrets.ForceFileBackend())
	if err != nil {
		t.Fatalf("LoadSetup after persistSetup: %v", err)
	}
	if len(got.Brokers) != 1 || got.Brokers[0].Kind != want.Brokers[0].Kind {
		t.Errorf("broker did not round-trip: %+v", got.Brokers)
	}
	if got.Brokers[0].Key != want.Brokers[0].Key || got.Brokers[0].Secret != want.Brokers[0].Secret {
		t.Errorf("broker creds did not round-trip: %+v", got.Brokers[0])
	}
	if got.Playbook.Capital.Cmp(want.Playbook.Capital) != 0 {
		t.Errorf("playbook capital did not round-trip: got %s want %s", got.Playbook.Capital, want.Playbook.Capital)
	}
	if got.Playbook.RiskTolerance != want.Playbook.RiskTolerance {
		t.Errorf("playbook risk tolerance did not round-trip: %q", got.Playbook.RiskTolerance)
	}
	if got.Live {
		t.Error("loaded setup must be PAPER (Live=false) by default")
	}
}

// TestRunDispatchesDashboardWhenConfigExists proves the REAL entry-point dispatch: when
// a config file is present, run() takes the DASHBOARD path (not the wizard). It swaps the
// injectable runWizardFn/runDashboardFn for spies so no real TUI is launched.
func TestRunDispatchesDashboardWhenConfigExists(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "dispatch-pass"
	if err := persistSetup(validSetupResult(t), configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("persistSetup: %v", err)
	}

	var wizardCalled, dashboardCalled bool
	restore := swapEntryFns(
		func(string) error { wizardCalled = true; return nil },
		func(string) error { dashboardCalled = true; return nil },
	)
	defer restore()

	if err := run([]string{"--config", configPath}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if wizardCalled {
		t.Error("config exists but the WIZARD was launched")
	}
	if !dashboardCalled {
		t.Error("config exists but the DASHBOARD was not launched")
	}
}

// TestRunDispatchesWizardWhenConfigAbsent proves the inverse: with no config file, run()
// takes the WIZARD path (first-run setup), never the dashboard.
func TestRunDispatchesWizardWhenConfigAbsent(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml") // never created

	var wizardCalled, dashboardCalled bool
	restore := swapEntryFns(
		func(string) error { wizardCalled = true; return nil },
		func(string) error { dashboardCalled = true; return nil },
	)
	defer restore()

	if err := run([]string{"--config", configPath}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if dashboardCalled {
		t.Error("no config but the DASHBOARD was launched")
	}
	if !wizardCalled {
		t.Error("no config but the WIZARD was not launched")
	}
}

// swapEntryFns installs spy wizard/dashboard runners and returns a restore func. It is a
// test helper so the dispatch tests read clearly and always restore the real funcs.
func swapEntryFns(wizard, dashboard func(string) error) func() {
	prevW, prevD := runWizardFn, runDashboardFn
	runWizardFn, runDashboardFn = wizard, dashboard
	return func() { runWizardFn, runDashboardFn = prevW, prevD }
}

// TestBuildDashboardFromConfigLoadsRealState proves the dashboard OPENS showing the
// owner's saved setup, not an empty stub: buildDashboardFromConfig loads the persisted
// config and produces an initial snapshot that carries the right mode (paper), the loaded
// equity (the playbook capital), and is non-empty (flat is fine — no positions yet).
func TestBuildDashboardFromConfigLoadsRealState(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bucks.yaml")
	const pass = "dash-build-pass"
	want := validSetupResult(t)
	if err := persistSetup(want, configPath, pass, secrets.ForceFileBackend()); err != nil {
		t.Fatalf("persistSetup: %v", err)
	}

	model, snap, _, err := buildDashboardFromConfig(configPath, pass, secrets.ForceFileBackend())
	if err != nil {
		t.Fatalf("buildDashboardFromConfig: %v", err)
	}

	// Mode reflects the saved PAPER setup (Live=false), not live.
	if snap.Health.Live {
		t.Error("snapshot mode must be PAPER (Live=false) from the loaded setup")
	}
	// Equity reflects the loaded playbook capital — the dashboard shows the owner's real
	// account, not a zero stub.
	if snap.Report.Equity.Cmp(want.Playbook.Capital) != 0 {
		t.Errorf("snapshot equity = %s, want loaded capital %s", snap.Report.Equity, want.Playbook.Capital)
	}
	// No positions yet on first open — that's the honest flat state, not an error.
	if len(snap.Report.Positions) != 0 {
		t.Errorf("fresh dashboard should be flat, got %d positions", len(snap.Report.Positions))
	}

	// The model must actually carry the snapshot (it OPENS populated, not "waiting…").
	got, ok := model.CurrentSnapshot()
	if !ok {
		t.Fatal("dashboard model has no snapshot — it would open empty")
	}
	if got.Report.Equity.Cmp(want.Playbook.Capital) != 0 {
		t.Errorf("model snapshot equity = %s, want %s", got.Report.Equity, want.Playbook.Capital)
	}
}
