package main

import (
	"fmt"
	"io"
	"os"

	"bucks/internal/tui"
)

// runLogoStdio is the production `bucks logo` (alias `bucks mascot`) entry point: it
// prints the fully-colored buck mascot to stdout so the owner can preview the
// terminal art, then exits 0. It performs no trade/risk/IO decisions and needs no
// config — it is a pure preview of the brand mark.
func runLogoStdio() error {
	return runLogo(os.Stdout)
}

// runLogo writes the colored buck banner to out. The writer is injected so the
// default suite can drive the real entry point and assert on the output offline.
func runLogo(out io.Writer) error {
	fmt.Fprintln(out, tui.RenderBanner())
	return nil
}
