package main

import (
	"path/filepath"
	"testing"

	"bucks/internal/risk"
)

// TestDaemonCommandContextHaltResumeStatus proves the durable CommandContext: Halt trips
// the real kill switch (durable, MANUAL kind), Status reflects the halt, and Resume clears
// it — all backed by a real on-disk KillSwitch (no fake). It also asserts the HONEST
// config-derived fields (mode/broker/equity) come straight from the loaded setup, not
// invented numbers, and that no positions are fabricated.
func TestDaemonCommandContextHaltResumeStatus(t *testing.T) {
	ksPath := filepath.Join(t.TempDir(), "killswitch.json")
	ks, err := risk.Open(ksPath)
	if err != nil {
		t.Fatalf("open kill switch: %v", err)
	}

	r := validSetupResult(t) // paper, broker alpaca-paper, capital 25000
	cc := newDaemonCommandContext(ks, r)

	// Fresh: not halted.
	if st := cc.Status(); st.Halted {
		t.Fatal("fresh kill switch must not be halted")
	}

	// HONEST config-derived status fields.
	st := cc.Status()
	if st.Mode != "paper" {
		t.Errorf("Mode = %q, want paper", st.Mode)
	}
	if st.Broker == "" || st.Broker == "unknown" {
		t.Errorf("Broker = %q, want the configured broker kind", st.Broker)
	}
	if st.Equity != r.Playbook.Capital.String() {
		t.Errorf("Equity = %q, want the playbook capital %q", st.Equity, r.Playbook.Capital.String())
	}

	// Halt trips the durable switch.
	if err := cc.Halt("operator /halt via Telegram"); err != nil {
		t.Fatalf("Halt: %v", err)
	}
	if halted, _ := ks.IsHalted(); !halted {
		t.Fatal("Halt did not trip the durable kill switch")
	}
	if ks.Kind() != risk.HaltManual {
		t.Errorf("halt kind = %s, want Manual", ks.Kind())
	}
	if st := cc.Status(); !st.Halted || st.HaltReason == "" {
		t.Errorf("Status after Halt = %+v, want Halted with a reason", st)
	}

	// The durable file truly carries the halt (survives a reopen) — no fabrication.
	reopened, err := risk.Open(ksPath)
	if err != nil {
		t.Fatalf("reopen kill switch: %v", err)
	}
	if halted, _ := reopened.IsHalted(); !halted {
		t.Error("halt did not persist to disk (would not survive a restart)")
	}

	// Resume clears it.
	if err := cc.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if halted, _ := ks.IsHalted(); halted {
		t.Fatal("Resume did not clear the kill switch")
	}
	if st := cc.Status(); st.Halted {
		t.Error("Status after Resume still reports halted")
	}
}

// TestDaemonCommandContextReportNoFabrication proves Report carries the real equity (the
// playbook capital) and does NOT invent positions or P&L (the live trade loop is not
// running yet, so the honest snapshot is flat).
func TestDaemonCommandContextReportNoFabrication(t *testing.T) {
	ksPath := filepath.Join(t.TempDir(), "killswitch.json")
	ks, err := risk.Open(ksPath)
	if err != nil {
		t.Fatalf("open kill switch: %v", err)
	}
	r := validSetupResult(t)
	cc := newDaemonCommandContext(ks, r)

	rep := cc.Report()
	if rep.Equity.Cmp(r.Playbook.Capital) != 0 {
		t.Errorf("Report equity = %s, want playbook capital %s", rep.Equity, r.Playbook.Capital)
	}
	if len(rep.Positions) != 0 {
		t.Errorf("Report must not fabricate positions, got %d", len(rep.Positions))
	}
	if rep.RealizedPL.Sign() != 0 || rep.UnrealizedPL.Sign() != 0 {
		t.Errorf("Report must not fabricate P&L, got realized=%s unrealized=%s", rep.RealizedPL, rep.UnrealizedPL)
	}
}
