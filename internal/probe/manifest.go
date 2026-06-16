// Package probe is BUCKS's capability-probe layer: it learns what a non-Alpaca
// broker API can do by reading the venue's own OpenAPI/Swagger spec, then proving
// that spec against the live server using ONLY read-only HTTP methods, and it
// keeps every write (order placement) physically LOCKED until the surface is
// proven, a paper/dry-run succeeds, and the operator confirms in plain English.
//
// The whole point is safety: reads can run autonomously (you can't lose money by
// reading), but a probe must never accidentally place, cancel, or mutate an order
// while it is "learning" an API. That guarantee is enforced in CODE — the
// read-only client (readonly.go) physically refuses to issue anything other than
// GET/HEAD/OPTIONS — not merely asserted in a test.
//
// Pipeline (see Probe.Run):
//
//  1. Discovery        — find the spec via RFC 9727 well-known, then openapi.json,
//     openapi.yaml, then framework fallbacks. Absence of one is not an error.
//  2. Parse + normalize— parse Swagger 2.0 / OAS 3.x, resolve $refs (libopenapi).
//  3. Manifest         — each operation → typed Capability {method, path, params,
//     security, READ|WRITE access, broker semantics}; features → flags.
//  4. Read-only probe  — verify each capability live with GET/HEAD/OPTIONS only;
//     OPTIONS Allow enumerates methods; 404/405 = ABSENT, 401/403 = AUTH.
//  5. Double-grounded mapping — an injected mapper may PROPOSE a mapping for thin
//     specs, but a proposal is only promoted to the manifest if it validates
//     against BOTH the parsed spec AND a live read-only probe.
//  6. Write gate       — AssertWriteAllowed stays an error until read-probe +
//     paper/dry-run + operator-confirm are all satisfied.
package probe

import "strings"

// Access classifies an operation as read-only or state-changing. The split is
// purely by HTTP method per the spec: GET/HEAD/OPTIONS are READ (safe, idempotent,
// can't lose money); POST/PUT/PATCH/DELETE are WRITE (state-changing, gated).
type Access int

const (
	// AccessRead is a safe, idempotent, read-only operation (GET/HEAD/OPTIONS).
	AccessRead Access = iota
	// AccessWrite is a state-changing operation (POST/PUT/PATCH/DELETE) — gated.
	AccessWrite
)

// String renders the access class for logs and reports.
func (a Access) String() string {
	if a == AccessWrite {
		return "WRITE"
	}
	return "READ"
}

// classifyMethod maps an HTTP method to its access class. Unknown/empty methods
// are treated as WRITE — fail SAFE: an unrecognized verb is never assumed to be a
// harmless read.
func classifyMethod(method string) Access {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "GET", "HEAD", "OPTIONS":
		return AccessRead
	default:
		return AccessWrite
	}
}

// Provenance records how much we trust a capability: did we only see it in the
// spec, did a live read-only probe confirm it, or did an LLM mapper merely infer
// it (and so it must stay untrusted until double-grounded)?
type Provenance int

const (
	// ProvenanceInferred means a mapper proposed it but it is NOT yet grounded in
	// both the spec and a live probe — it must never be treated as real capability.
	ProvenanceInferred Provenance = iota
	// ProvenanceSpecOnly means the spec declares it but a live read-only probe has
	// not (yet) confirmed it against the running server.
	ProvenanceSpecOnly
	// ProvenanceVerified means a live read-only probe confirmed the surface exists
	// (method enumerated via OPTIONS Allow, or a GET/HEAD returned a non-absent
	// status). This is the only level the write gate will accept as "proven".
	ProvenanceVerified
)

// String renders provenance for reports.
func (p Provenance) String() string {
	switch p {
	case ProvenanceVerified:
		return "verified"
	case ProvenanceSpecOnly:
		return "spec-only"
	default:
		return "inferred"
	}
}

// Semantic tags a capability with the broker concept it most likely serves, so
// strategy/risk code can find "the quote endpoint" without hard-coding a venue's
// URLs. SemanticUnknown means we could not recognize it (still a valid capability,
// just untagged).
type Semantic int

const (
	// SemanticUnknown is an operation we could not map to a known broker concept.
	SemanticUnknown Semantic = iota
	// SemanticAccount is account/balance/equity/buying-power.
	SemanticAccount
	// SemanticPositions is open positions / holdings / portfolio.
	SemanticPositions
	// SemanticOrders is order placement / cancel / status.
	SemanticOrders
	// SemanticQuote is market-data / quotes / prices / ticker.
	SemanticQuote
)

// String renders the semantic tag.
func (s Semantic) String() string {
	switch s {
	case SemanticAccount:
		return "account"
	case SemanticPositions:
		return "positions"
	case SemanticOrders:
		return "orders"
	case SemanticQuote:
		return "quote"
	default:
		return "unknown"
	}
}

// classifySemantic guesses the broker concept from an operation's path, tags and
// summary using conservative keyword matching. It is best-effort: an unrecognized
// operation stays SemanticUnknown (never mis-tagged), and order-ish wins over the
// others because placing/cancelling orders is the surface we most care about
// gating.
func classifySemantic(path string, tags []string, summary string) Semantic {
	hay := strings.ToLower(path + " " + strings.Join(tags, " ") + " " + summary)
	switch {
	case containsAny(hay, "order", "/trade", "trades"):
		return SemanticOrders
	case containsAny(hay, "position", "holding", "portfolio"):
		return SemanticPositions
	case containsAny(hay, "account", "balance", "equity", "buying_power", "buyingpower"):
		return SemanticAccount
	case containsAny(hay, "quote", "ticker", "price", "/marketdata", "market_data", "best_bid"):
		return SemanticQuote
	default:
		return SemanticUnknown
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// Param is one operation parameter from the spec.
type Param struct {
	Name     string // parameter name
	In       string // location: "query", "path", "header", "cookie", "body"
	Required bool   // whether the spec marks it required
}

// Capability is one typed operation discovered for a broker API: an HTTP method +
// path, its parameters, the security schemes that guard it, its READ/WRITE access
// class, the broker concept it serves, and the provenance that says how much we
// trust it. The manifest is a list of these.
type Capability struct {
	Method      string     // uppercase HTTP method (GET, POST, ...)
	Path        string     // templated path (e.g. /v2/orders/{id})
	OperationID string     // spec operationId, if any (for diagnostics)
	Summary     string     // human summary from the spec
	Params      []Param    // declared parameters
	Security    []string   // names of security schemes that guard the op
	Access      Access     // READ vs WRITE
	Semantic    Semantic   // tagged broker concept
	Provenance  Provenance // spec-only / verified / inferred
}

// Feature flags summarize coarse capabilities of an API for quick lookups by
// strategy/risk: which asset classes, order types, and modes a venue advertises.
// They are derived from the operations and (when present) the spec's schemas.
type Features struct {
	AssetClasses map[string]bool // e.g. "crypto", "equity", "option"
	OrderTypes   map[string]bool // e.g. "market", "limit"
	PaperMode    bool            // venue advertises a paper/sandbox surface
	WebSocket    bool            // venue advertises a streaming/websocket surface
}

// Manifest is the typed capability map for one broker API: every operation, the
// derived feature flags, and which OpenAPI/Swagger version it came from.
type Manifest struct {
	SpecVersion  string       // e.g. "3.0.3", "2.0"
	Capabilities []Capability // every discovered operation
	Features     Features     // derived feature flags
}

// Reads returns only the READ capabilities (safe to probe/run live).
func (m *Manifest) Reads() []Capability {
	var out []Capability
	for _, c := range m.Capabilities {
		if c.Access == AccessRead {
			out = append(out, c)
		}
	}
	return out
}

// Writes returns only the WRITE capabilities (gated — never run during probing).
func (m *Manifest) Writes() []Capability {
	var out []Capability
	for _, c := range m.Capabilities {
		if c.Access == AccessWrite {
			out = append(out, c)
		}
	}
	return out
}

// FindBySemantic returns capabilities tagged with the given broker concept.
func (m *Manifest) FindBySemantic(s Semantic) []Capability {
	var out []Capability
	for _, c := range m.Capabilities {
		if c.Semantic == s {
			out = append(out, c)
		}
	}
	return out
}
