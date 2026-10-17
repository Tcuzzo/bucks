package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"bucks/internal/analyst"
	"bucks/internal/understanding"
)

// runUnderstandStdio is the production `bucks understand <file> "<intent>"` entry
// point: it reads the candidate file, resolves a backend from the SAME BUCKS_CHAT_*
// env the chat/research/summary commands use (with the grading token budget raised),
// and prints the evidence — dimension scores, named failures, recovery actions.
//
// It is ADVISORY. It grades BUILDS, never trades: no order, risk, or execution path
// consults it, and it exits 0 whether the verdict approves or rejects. With no
// backend it prints setup guidance and exits 0 — never a crash, never a gate.
func runUnderstandStdio(args []string) error {
	path := ""
	if len(args) > 0 {
		path = args[0]
	}
	intent := strings.TrimSpace(strings.Join(args[1:], " "))
	return runUnderstand(os.Stdout, envUnderstandBackends, path, intent)
}

// envUnderstandBackends shares the SINGLE backend-selection seam (envChatBackend)
// with chat, research, and summary — so a BUCKS_CHAT_PROVIDER choice applies here
// identically — while raising the completion budget to understanding.MinGradingTokens.
//
// That raise is the whole reason this factory exists rather than calling
// envChatBackend directly: BUCKS's free default brain is a REASONING model, and the
// backend's stock budget is sized for a short chat reply. A grading reply is a long
// structured payload the model must produce AFTER thinking, out of the SAME budget —
// starve it and `content` comes back empty. Returns (nil, nil) when nothing is
// configured (the clean no-backend case).
func envUnderstandBackends() ([]analyst.Backend, error) {
	backend, err := envChatBackend(analyst.WithMaxTokens(understanding.MinGradingTokens))
	if err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, nil
	}
	return []analyst.Backend{backend}, nil
}

// runUnderstand grades one candidate file against the stated intent and prints the
// evidence using the injected backends factory. out + the factory are injected so
// the default suite drives the real entry point offline.
//
// The honesty contract, end to end: an unreadable file is a loud error; a missing
// model prints guidance that says plainly it is NOT a pass; a refutation prints in
// full. Nothing here can print an approval that the organ did not compute.
func runUnderstand(out io.Writer, newBackends backendsFactory, path, intent string) error {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(intent) == "" {
		fmt.Fprintln(out, "bucks understand: usage: bucks understand <file> \"<what it was supposed to do>\"")
		fmt.Fprintln(out, "  Grades a candidate against your stated intent and shows the evidence. Advisory — it blocks nothing.")
		return nil
	}
	code, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("understand: cannot read candidate %s: %w", path, err)
	}
	backends, err := newBackends()
	if err != nil {
		return fmt.Errorf("understand: %w", err)
	}

	a, err := understanding.Grade(context.Background(), backends, understanding.Candidate{
		Name:   path,
		Intent: intent,
		Code:   string(code),
	})
	if err != nil {
		// Advisory, never fatal: an absent or failing model is REPORTED in plain
		// English and exits 0. What it must never do is read as approval — which is
		// why printAssessment leads with the UNAVAILABLE verdict either way.
		if !errors.Is(err, understanding.ErrNoBackend) {
			fmt.Fprintf(out, "bucks understand: could not grade %s: %v\n\n", path, err)
		}
		printAssessment(out, a)
		return nil
	}
	printAssessment(out, a)
	return nil
}

// printAssessment writes the organ's evidence in plain English. It prints what the
// Assessment actually holds and nothing more — an ungraded candidate shows its
// UNAVAILABLE verdict and its setup guidance, never a score.
func printAssessment(out io.Writer, a understanding.Assessment) {
	if a.Status != understanding.StatusGraded {
		fmt.Fprintf(out, "%s — %s was not checked.\n\n", a.Verdict, a.Candidate)
		fmt.Fprintln(out, strings.TrimSpace(a.Guidance))
		return
	}

	fmt.Fprintf(out, "%s — %s scored %d/100\n\n", a.Verdict, a.Candidate, a.Total)
	fmt.Fprintln(out, "Where it stands:")
	for _, d := range understanding.Dimensions {
		fmt.Fprintf(out, "  %-18s %d/%d\n", d, a.Scores[d], understanding.MaxDimensionScore)
	}
	if len(a.Failures) > 0 {
		fmt.Fprintln(out, "\nWhat it got wrong:")
		for _, f := range a.Failures {
			fmt.Fprintf(out, "  - [%s] %s\n", f.Dimension, f.Detail)
			if f.Cite != "" {
				fmt.Fprintf(out, "      in your code: %s\n", f.Cite)
			}
			if !f.Grounded {
				// The model pointed at something that is not in the file. Say so —
				// an unbacked finding is never presented as established fact.
				fmt.Fprintln(out, "      (unverified: that text is NOT in the file — treat this finding with suspicion)")
			}
		}
	}
	if len(a.RecoveryActions) > 0 {
		fmt.Fprintln(out, "\nWhat to do about it:")
		for _, r := range a.RecoveryActions {
			fmt.Fprintf(out, "  - %s\n", r)
		}
	}
	fmt.Fprintf(out, "\nGraded by %s (model confidence %.0f%%). Advisory only — nothing is blocked.\n",
		a.Backend, a.Confidence*100)
	if a.Downgraded() {
		fmt.Fprintf(out, "(note: graded on fallback model %q after %d failover(s))\n",
			a.Backend, len(a.Failovers))
	}
}
