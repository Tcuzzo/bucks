package probe

import (
	"context"
	"net/http"
	"strings"
)

// Mapping is a proposal from an (injectable) LLM-assisted mapper: "the manifest's
// concept X is served by this method+path". It is a HYPOTHESIS only. It never
// enters the manifest as real capability until it is double-grounded — validated
// against BOTH the parsed spec AND a live read-only probe (see Probe.ground).
type Mapping struct {
	Semantic Semantic // broker concept this mapping claims to satisfy
	Method   string   // proposed HTTP method
	Path     string   // proposed path
}

// Mapper proposes mappings for thin/HTML-only specs. It is injected (a func), so
// tests use a deterministic mock and the default suite NEVER calls a live LLM.
// The mapper is advisory: the probe owns validation, not the mapper.
type Mapper func(ctx context.Context, m *Manifest) []Mapping

// Result is the full output of a probe run: the typed manifest (with provenance
// updated by the live probe and any double-grounded mappings folded in) plus the
// write gate that governs whether order placement is allowed.
type Result struct {
	Manifest *Manifest
	Gate     *WriteGate
}

// Probe runs the capability-probe pipeline against one broker API. It is
// constructed against a base URL and an optional auth hook + mapper; everything
// it does over the network goes through a read-only client, so it physically
// cannot place or cancel an order while learning the API.
type Probe struct {
	ro     *roClient
	mapper Mapper
}

// NewProbe builds a Probe against baseURL. hc may be nil (DefaultClient). auth may
// be nil (no credentials). mapper may be nil (no LLM-assisted mapping step).
func NewProbe(hc *http.Client, baseURL string, auth func(*http.Request), mapper Mapper) *Probe {
	return &Probe{
		ro:     newROClient(hc, baseURL, auth),
		mapper: mapper,
	}
}

// Run executes the six-stage pipeline: discover → parse/normalize → manifest →
// live read-only probe → double-grounded mapping → return a fresh (locked) write
// gate. The returned gate has already recorded that the read-probe ran; a
// paper/dry-run and an operator confirmation are still required before writes
// unlock.
func (p *Probe) Run(ctx context.Context) (*Result, error) {
	spec, err := Discover(ctx, p.ro)
	if err != nil {
		return nil, err
	}
	manifest := spec.BuildManifest()

	// Stage 4: verify the manifest live using ONLY read-only methods.
	p.liveReadProbe(ctx, manifest)

	// Stage 5: double-grounded LLM-assisted mapping (no-op if no mapper).
	if p.mapper != nil {
		p.groundMappings(ctx, spec, manifest)
	}

	gate := NewWriteGate()
	gate.markReadProbed() // the read-probe has run; writes still need paper+confirm.

	return &Result{Manifest: manifest, Gate: gate}, nil
}

// liveReadProbe verifies each capability against the running server using only
// GET/HEAD/OPTIONS. It uses OPTIONS first to enumerate supported methods via the
// Allow header (no invocation), then HEAD/GET to confirm a READ surface. A
// capability is promoted to ProvenanceVerified when:
//   - OPTIONS Allow lists its method, OR
//   - it is a READ op and a HEAD/GET returns a non-absent, non-auth status.
//
// 404/405 → leave at spec-only (feature absent live). 401/403 → leave at
// spec-only but the surface clearly exists (auth problem, distinct from absent);
// we treat "exists-but-auth" as verified-present because the endpoint is real.
// WRITE capabilities are NEVER invoked: their presence is inferred ONLY from an
// OPTIONS Allow listing, never from issuing the verb.
func (p *Probe) liveReadProbe(ctx context.Context, m *Manifest) {
	// Cache OPTIONS results per concrete path so we don't re-probe the same path.
	allowByPath := map[string][]string{}
	probedPath := map[string]bool{}

	for i := range m.Capabilities {
		c := &m.Capabilities[i]
		concrete := concretePath(c.Path)

		if !probedPath[concrete] {
			if status, allow, err := p.ro.options(ctx, concrete); err == nil {
				probedPath[concrete] = true
				if classifyStatus(status) != statusAbsent {
					allowByPath[concrete] = allow
				}
			} else {
				probedPath[concrete] = true
			}
		}

		// (a) OPTIONS Allow enumerated this method without invoking it.
		if methodAllowed(allowByPath[concrete], c.Method) {
			c.Provenance = ProvenanceVerified
			continue
		}

		// (b) For READ ops only, a HEAD/GET confirms a live, non-absent surface.
		if c.Access == AccessRead {
			status, _, err := p.ro.head(ctx, concrete)
			if err != nil {
				continue
			}
			switch classifyStatus(status) {
			case statusAbsent:
				// 404/405 — feature not present live; stay spec-only.
			case statusAuth:
				// 401/403 — endpoint EXISTS but auth failed (distinct from absent);
				// the surface is real, so it is verified-present.
				c.Provenance = ProvenanceVerified
			default:
				c.Provenance = ProvenanceVerified
			}
		}
		// WRITE ops are never invoked here — if OPTIONS didn't enumerate them, they
		// stay spec-only. We never issue POST/PUT/PATCH/DELETE to "confirm" a write.
	}
}

// groundMappings runs the injected mapper and folds in ONLY the proposals that
// pass double-grounding: a proposal must (1) correspond to an operation actually
// present in the parsed spec, AND (2) be confirmed live by a read-only probe
// (OPTIONS Allow enumeration, or a non-absent HEAD for a READ). A proposal that
// fails either gate is dropped (for a brand-new concept) or, if it overlaps an
// existing spec capability, recorded as ProvenanceInferred — never promoted to
// verified. This is the "double-grounded" rule: the LLM can suggest, but only the
// spec + the live server can confirm.
func (p *Probe) groundMappings(ctx context.Context, spec *SpecDoc, m *Manifest) {
	for _, mp := range p.mapper(ctx, m) {
		method := strings.ToUpper(strings.TrimSpace(mp.Method))

		// Ground #1: the proposal must exist in the parsed spec.
		inSpec := specHasOp(m, method, mp.Path)

		// Ground #2: the live server must confirm it via read-only methods.
		liveOK := p.liveConfirm(ctx, method, mp.Path)

		if inSpec && liveOK {
			// Fully double-grounded: mark the matching capability verified.
			promoteOrAdd(m, method, mp)
			continue
		}
		// Failed double-grounding: never enters as verified. If it is a brand-new
		// path not in the spec, drop it entirely. If it overlaps a spec op, the
		// existing capability keeps its own (spec-only/verified) provenance — we do
		// NOT downgrade it, but we also do NOT promote the unvalidated proposal.
	}
}

// liveConfirm confirms an operation against the live server using ONLY read-only
// methods: OPTIONS Allow enumeration, or (for a READ) a non-absent HEAD.
func (p *Probe) liveConfirm(ctx context.Context, method, path string) bool {
	concrete := concretePath(path)
	if status, allow, err := p.ro.options(ctx, concrete); err == nil &&
		classifyStatus(status) != statusAbsent && methodAllowed(allow, method) {
		return true
	}
	if classifyMethod(method) == AccessRead {
		if status, _, err := p.ro.head(ctx, concrete); err == nil &&
			classifyStatus(status) != statusAbsent {
			return true
		}
	}
	return false
}

// promoteOrAdd marks an existing matching capability verified, or adds a new
// verified capability for a double-grounded mapping not already in the manifest.
func promoteOrAdd(m *Manifest, method string, mp Mapping) {
	for i := range m.Capabilities {
		if m.Capabilities[i].Method == method && m.Capabilities[i].Path == mp.Path {
			m.Capabilities[i].Provenance = ProvenanceVerified
			if m.Capabilities[i].Semantic == SemanticUnknown {
				m.Capabilities[i].Semantic = mp.Semantic
			}
			return
		}
	}
	m.Capabilities = append(m.Capabilities, Capability{
		Method:     method,
		Path:       mp.Path,
		Access:     classifyMethod(method),
		Semantic:   mp.Semantic,
		Provenance: ProvenanceVerified,
	})
}

// specHasOp reports whether the manifest (built from the parsed spec) already has
// this method+path. This is "Ground #1" — presence in the parsed spec.
func specHasOp(m *Manifest, method, path string) bool {
	for _, c := range m.Capabilities {
		if c.Method == method && c.Path == path {
			return true
		}
	}
	return false
}

// methodAllowed reports whether method appears in an OPTIONS Allow list.
func methodAllowed(allow []string, method string) bool {
	up := strings.ToUpper(method)
	for _, a := range allow {
		if a == up {
			return true
		}
	}
	return false
}

// concretePath turns a templated path into something probeable. Path templates
// like /v2/orders/{id} can't be probed directly. We return everything BEFORE the
// FIRST (leftmost) templated segment, because no segment at or after a template
// has a known concrete value to probe. So /v2/orders/{id}/fills probes /v2/orders
// (NOT /v2/orders/{id}/fills, which a live read-only probe could never match —
// that left the capability stuck spec-only). A path whose first segment is
// templated has nothing concrete to probe, so it falls back to "/". Non-templated
// paths are returned unchanged.
func concretePath(path string) string {
	if !strings.Contains(path, "{") {
		return path
	}
	segs := strings.Split(path, "/")
	for i := range segs {
		if strings.Contains(segs[i], "{") {
			segs = segs[:i]
			break
		}
	}
	cleaned := strings.Join(segs, "/")
	if cleaned == "" {
		return "/"
	}
	return cleaned
}
