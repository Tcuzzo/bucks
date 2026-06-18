package main

import (
	"fmt"
	"io"
)

// helpText is the top-level usage shown by `bucks help`, `bucks -h`, `bucks --help`,
// and on an unknown command. It lists EVERY command run() dispatches plus a one-line
// description, so the positional subcommands (which are dispatched before flag
// parsing, and so never appear in the flag usage) are discoverable. The command set
// here is exactly the one in main.go's dispatch — no invented commands.
const helpText = `BUCKS — an autonomous trading agent. Usage: bucks <command> [flags]

Run:
  bucks                 open the live dashboard (or first-run setup wizard)
  bucks --daemon        run headless under a service manager
Talk:
  bucks chat            talk to BUCKS like a person
  bucks summary         plain-English summary of your positions / P&L
  bucks research "<q>"  read-only web research on a topic
  bucks read <url>      fetch + summarize a page
Manage:
  bucks version         print the version
  bucks update          update to the latest release (SHA-256 verified)
  bucks logo            show the brand mark
Flags: --config <path>, --chat, --paper-smoke (see ` + "`bucks <command> --help`" + `).
`

// runHelp writes the top-level help to out and returns nil (exit 0 for the explicit
// help commands). The writer is injected so the default suite can drive the real
// entry point and assert on the output offline; callers route it to stdout (explicit
// help) or stderr (unknown command).
func runHelp(out io.Writer) error {
	fmt.Fprint(out, helpText)
	return nil
}
