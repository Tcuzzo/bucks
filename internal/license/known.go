package license

// KnownLicenses is BUCKS's CURATED, human-reviewed module -> SPDX-id map. Each entry
// was read off the dependency's actual LICENSE file in the module cache (the SPDX id
// reflects that file's real header), then recorded here. This is the authoritative
// source the ship-time gate (ScanCurated) classifies — NOT a runtime heuristic.
//
// The set mirrors the modules declared in go.mod's require blocks (direct + the
// indirect tree BUCKS links). When a dependency is added, its real license MUST be
// read and an entry added here; the cross-check test
// (TestEveryGoModRequireIsCurated) fails the build until that happens, so an
// unreviewed license can never slip into the ship.
//
// SPDX ids only — Apache-2.0/MIT/BSD-*/ISC are permissive and ship-safe; there is
// (by design and by the scan) NO (A)GPL/LGPL/MPL anywhere in this map. If one ever
// appears here, the gate HARD-FAILS rather than ship it.
var KnownLicenses = map[string]string{
	// --- direct, named in the spec / used by BUCKS ---
	"github.com/alpacahq/alpaca-trade-api-go/v3": "Apache-2.0",
	"github.com/charmbracelet/bubbletea":         "MIT",
	"github.com/charmbracelet/lipgloss":          "MIT",
	"github.com/govalues/decimal":                "MIT",
	"github.com/pb33f/libopenapi":                "MIT",
	"github.com/shopspring/decimal":              "MIT",
	"github.com/zalando/go-keyring":              "MIT",
	"filippo.io/age":                             "BSD-3-Clause",
	"gopkg.in/yaml.v3":                           "MIT",
	"modernc.org/sqlite":                         "BSD-3-Clause",

	// --- charmbracelet TUI stack (MIT) ---
	"github.com/charmbracelet/colorprofile": "MIT",
	"github.com/charmbracelet/x/ansi":       "MIT",
	"github.com/charmbracelet/x/cellbuf":    "MIT",
	"github.com/charmbracelet/x/term":       "MIT",
	"github.com/aymanbagabas/go-osc52/v2":   "MIT",
	"github.com/clipperhouse/displaywidth":  "MIT",
	"github.com/clipperhouse/stringish":     "MIT",
	"github.com/clipperhouse/uax29/v2":      "MIT",
	"github.com/erikgeiser/coninput":        "MIT",
	"github.com/lucasb-eyer/go-colorful":    "MIT",
	"github.com/mattn/go-isatty":            "MIT",
	"github.com/mattn/go-localereader":      "MIT",
	"github.com/mattn/go-runewidth":         "MIT",
	"github.com/muesli/ansi":                "MIT",
	"github.com/muesli/cancelreader":        "MIT",
	"github.com/muesli/termenv":             "MIT",
	"github.com/rivo/uniseg":                "MIT",
	"github.com/xo/terminfo":                "MIT",

	// --- openapi / json stack ---
	"github.com/pb33f/jsonpath":        "Apache-2.0",
	"github.com/pb33f/ordered-map/v2":  "Apache-2.0",
	"github.com/bahlo/generic-list-go": "BSD-3-Clause",
	"github.com/buger/jsonparser":      "MIT",
	"github.com/dustin/go-humanize":    "MIT",
	"github.com/josharian/intern":      "MIT",
	"github.com/mailru/easyjson":       "MIT",
	"go.yaml.in/yaml/v4":               "Apache-2.0",

	// --- secrets-at-rest stack (keychain + age) ---
	"filippo.io/hpke":               "BSD-3-Clause",
	"github.com/danieljoos/wincred": "MIT",          // Windows Credential Manager backend
	"github.com/godbus/dbus/v5":     "BSD-2-Clause", // Linux Secret Service backend
	"golang.org/x/crypto":           "BSD-3-Clause",

	// --- misc indirect (all permissive) ---
	"cloud.google.com/go":              "Apache-2.0",
	"github.com/google/uuid":           "BSD-3-Clause",
	"github.com/ncruces/go-strftime":   "MIT",
	"github.com/remyoudompheng/bigfft": "BSD-3-Clause",
	"github.com/rogpeppe/go-internal":  "BSD-3-Clause",
	"github.com/kr/text":               "MIT",
	"golang.org/x/sync":                "BSD-3-Clause",
	"golang.org/x/sys":                 "BSD-3-Clause",
	"golang.org/x/term":                "BSD-3-Clause", // no-echo passphrase prompt (keychain-less first run)
	"golang.org/x/text":                "BSD-3-Clause",
	"modernc.org/libc":                 "BSD-3-Clause",
	"modernc.org/mathutil":             "BSD-3-Clause",
	"modernc.org/memory":               "BSD-3-Clause",
}
