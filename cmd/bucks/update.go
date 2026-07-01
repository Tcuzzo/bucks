package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"bucks/internal/updater"
)

// runVersionStdio is the production `bucks version` entry point: it prints the
// build-stamped version plus the platform and Go runtime, then exits 0. It needs no
// config and makes no network call.
func runVersionStdio() error {
	return runVersion(os.Stdout)
}

// runVersion writes the version line to out. The writer is injected so the default
// suite drives the real entry point and asserts the output offline.
func runVersion(out io.Writer) error {
	fmt.Fprintln(out, updater.VersionLine(runtime.Version(), runtime.GOOS, runtime.GOARCH))
	return nil
}

// runUpdateStdio is the production `bucks update` entry point. It parses the update
// flags, then runs the safe self-updater against stdin/stdout. A network failure is
// reported as a clear message and returns a non-zero error only on a real failure.
//
//	bucks update            check, then ask y/N before replacing
//	bucks update --yes/-y   replace without asking (still checksum-verified)
//	bucks update --check    report availability and exit (no download)
//	bucks update --force    reinstall even if not newer
func runUpdateStdio(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	checkOnly := fs.Bool("check", false, "only report whether an update is available, then exit")
	var assumeYes bool
	fs.BoolVar(&assumeYes, "yes", false, "do not prompt; apply the update")
	fs.BoolVar(&assumeYes, "y", false, "shorthand for --yes")
	force := fs.Bool("force", false, "reinstall even if the latest is not newer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	u := updater.New()
	return runUpdate(context.Background(), u, os.Stdin, os.Stdout, updateFlags{
		checkOnly: *checkOnly,
		assumeYes: assumeYes,
		force:     *force,
	})
}

// updateFlags are the parsed `bucks update` options, passed to the testable core.
type updateFlags struct {
	checkOnly bool
	assumeYes bool
	force     bool
}

// runUpdate is the testable update flow. The updater, the input reader (for the y/N
// prompt), and the output writer are all injected so the default suite drives the
// REAL entry point offline.
func runUpdate(ctx context.Context, u *updater.Updater, in io.Reader, out io.Writer, fl updateFlags) error {
	rel, err := u.CheckLatest(ctx)
	if err != nil {
		// Network failure → a clear, plain-English message; non-zero exit.
		fmt.Fprintf(out, "Could not check for updates: %v\n", err)
		return err
	}

	current := u.CurrentVersion()
	if rel.IsDevCur {
		fmt.Fprintf(out, "Current version: dev (un-versioned build)\n")
	} else {
		fmt.Fprintf(out, "Current version: %s\n", current)
	}
	fmt.Fprintf(out, "Latest release:  %s\n", rel.Tag)

	if !rel.IsNewer && !fl.force {
		fmt.Fprintln(out, "You are already up to date.")
		return nil
	}

	if fl.checkOnly {
		fmt.Fprintln(out, "An update is available. Run `bucks update` to install it.")
		return nil
	}

	fmt.Fprintf(out, "Update available: %s -> %s\n", displayCurrent(rel, current), rel.Tag)

	if !fl.assumeYes {
		fmt.Fprint(out, "Download and install this update? [y/N]: ")
		if !confirmYes(in) {
			fmt.Fprintln(out, "Update cancelled.")
			return nil
		}
	}

	fmt.Fprintln(out, "Downloading and verifying (SHA-256)...")
	res, err := u.Update(ctx, updater.Options{Force: fl.force, ExpectedTag: rel.Tag})
	if err != nil {
		fmt.Fprintf(out, "Update failed: %v\n", err)
		return err
	}
	fmt.Fprintln(out, res.Message)
	return nil
}

// displayCurrent renders the current version for the "X -> Y" line.
//
//nolint:unused // referenced via runUpdate.
func displayCurrent(rel updater.Release, current string) string {
	if rel.IsDevCur {
		return "dev"
	}
	return current
}

// confirmYes reads one line and returns true only for an explicit yes (y/yes). EOF or
// anything else is treated as "no" — fail SAFE (never install on an ambiguous answer).
func confirmYes(in io.Reader) bool {
	r := bufio.NewReader(in)
	line, _ := r.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}
