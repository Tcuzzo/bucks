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
  bucks --daemon        run headless under a service manager (always-on Telegram gateway)
  bucks --paper-smoke   boot the saved config + place one in-band paper trade, then exit

Talk:
  bucks chat            talk to BUCKS like a person
  bucks summary         plain-English summary of your positions / P&L
  bucks research "<q>"  read-only web research on a topic
  bucks read <url>      fetch + summarize a web page

Manage:
  bucks version         print the version + build info
  bucks update          update to the latest release (SHA-256 verified)
  bucks logo            show the brand mark   (alias: bucks mascot)
  bucks help            show this help        (also: bucks -h, bucks --help)

Flags:
  --config <path>       use a specific config file
  --chat                open the chat REPL (same as: bucks chat)
  --daemon              run headless, no TUI
  --live                arm REAL-MONEY trading this session (default: paper / monitor-only)
  --paper-smoke         offline paper-trade acceptance, then exit
  -h, --help            show this help
`

// runHelp writes the top-level help to out and returns nil (exit 0 for the explicit
// help commands). The writer is injected so the default suite can drive the real
// entry point and assert on the output offline; callers route it to stdout (explicit
// help) or stderr (unknown command).
func runHelp(out io.Writer) error {
	fmt.Fprint(out, helpText)
	return nil
}
