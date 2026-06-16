package probe

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/pb33f/libopenapi"
	v3base "github.com/pb33f/libopenapi/datamodel/high/base"
	v2high "github.com/pb33f/libopenapi/datamodel/high/v2"
	v3high "github.com/pb33f/libopenapi/datamodel/high/v3"
)

// ErrSpecNotFound is returned by Discover when none of the well-known discovery
// locations yielded a parseable OpenAPI/Swagger document. It is a CLEAR not-found
// — distinct from a transport error — so callers can fall back to LLM-assisted
// mapping rather than treating a missing spec as a crash.
var ErrSpecNotFound = errors.New("probe: no OpenAPI/Swagger spec found at any discovery location")

// discoveryPaths is the priority-ordered list of read-only locations the probe
// tries to find a machine-readable API description. Per RFC 9727 the well-known
// api-catalog comes first, then the conventional OpenAPI document names, then
// framework defaults (springdoc emits /v3/api-docs; many emit /swagger.json).
// Absence of any one is NOT an error — the probe just moves to the next.
var discoveryPaths = []string{
	"/.well-known/api-catalog", // RFC 9727
	"/openapi.json",
	"/openapi.yaml",
	"/swagger.json", // common framework fallback
	"/v3/api-docs",  // springdoc default
}

// SpecDoc is a discovered, parsed, $ref-resolved API description plus the path it
// was found at and its raw bytes (kept for double-grounding against the spec).
type SpecDoc struct {
	FoundAt string           // discovery path the spec came from
	Version string           // exact spec version (e.g. "3.0.3", "2.0")
	Raw     []byte           // raw spec bytes
	v3      *v3high.Document // built OAS 3.x model ($refs resolved), or nil
	v2      *v2high.Swagger  // built Swagger 2.0 model ($refs resolved), or nil
}

// Discover probes baseURL in priority order using ONLY read-only GETs and returns
// the first location whose body parses as a valid OpenAPI/Swagger document. If a
// location 404s (absent) it tries the next; only a parseable spec stops the
// search. When nothing parses anywhere it returns ErrSpecNotFound.
func Discover(ctx context.Context, ro *roClient) (*SpecDoc, error) {
	for _, p := range discoveryPaths {
		status, body, _, err := ro.get(ctx, p)
		if err != nil {
			// Transport failure on one path shouldn't abort discovery; try the next.
			continue
		}
		if classifyStatus(status) == statusAbsent || len(body) == 0 {
			continue // absence is not an error
		}
		if status < 200 || status >= 400 {
			continue
		}
		doc, perr := parseSpec(p, body)
		if perr != nil {
			// A 200 that isn't a valid spec (e.g. an HTML page) — not the spec we
			// want; keep looking.
			continue
		}
		return doc, nil
	}
	return nil, ErrSpecNotFound
}

// parseSpec parses raw spec bytes (JSON or YAML) into a $ref-resolved model. It
// auto-detects Swagger 2.0 vs OAS 3.x and builds the matching high-level model;
// libopenapi's high model dereferences $refs, giving us a fully-inlined view.
func parseSpec(foundAt string, raw []byte) (*SpecDoc, error) {
	d, err := libopenapi.NewDocument(raw)
	if err != nil {
		return nil, fmt.Errorf("probe: parse spec at %s: %w", foundAt, err)
	}
	sd := &SpecDoc{FoundAt: foundAt, Version: d.GetVersion(), Raw: raw}

	info := d.GetSpecInfo()
	switch {
	case info != nil && info.SpecType == "openapi":
		model, merr := d.BuildV3Model()
		if merr != nil {
			return nil, fmt.Errorf("probe: build OAS3 model at %s: %w", foundAt, merr)
		}
		sd.v3 = &model.Model
	case info != nil && info.SpecType == "swagger":
		model, merr := d.BuildV2Model()
		if merr != nil {
			return nil, fmt.Errorf("probe: build Swagger2 model at %s: %w", foundAt, merr)
		}
		sd.v2 = &model.Model
	default:
		// Fall back to version sniffing if SpecType is unset.
		if v3model, merr := d.BuildV3Model(); merr == nil {
			sd.v3 = &v3model.Model
		} else if v2model, v2err := d.BuildV2Model(); v2err == nil {
			sd.v2 = &v2model.Model
		} else {
			return nil, fmt.Errorf("probe: spec at %s is neither valid OAS3 nor Swagger2: %w", foundAt, errors.Join(merr, v2err))
		}
	}
	return sd, nil
}

// BuildManifest walks the parsed, $ref-resolved spec and produces the typed
// capability manifest: one Capability per (path, method) operation, classified
// READ/WRITE, semantically tagged, with derived feature flags. Every capability
// starts at ProvenanceSpecOnly; the live read-only probe promotes the ones it can
// confirm to ProvenanceVerified.
func (s *SpecDoc) BuildManifest() *Manifest {
	m := &Manifest{
		SpecVersion: s.Version,
		Features: Features{
			AssetClasses: map[string]bool{},
			OrderTypes:   map[string]bool{},
		},
	}
	if s.v3 != nil {
		s.buildFromV3(m)
	} else if s.v2 != nil {
		s.buildFromV2(m)
	}
	deriveOrderTypes(m)
	return m
}

func (s *SpecDoc) buildFromV3(m *Manifest) {
	if s.v3.Paths == nil || s.v3.Paths.PathItems == nil {
		return
	}
	for path, item := range s.v3.Paths.PathItems.FromOldest() {
		for method, op := range item.GetOperations().FromOldest() {
			cap := Capability{
				Method:     normalizeMethod(method),
				Path:       path,
				Access:     classifyMethod(method),
				Provenance: ProvenanceSpecOnly,
			}
			if op != nil {
				cap.OperationID = op.OperationId
				cap.Summary = op.Summary
				cap.Params = v3Params(op.Parameters)
				cap.Security = v3Security(op.Security)
				cap.Semantic = classifySemantic(path, op.Tags, op.Summary)
			} else {
				cap.Semantic = classifySemantic(path, nil, "")
			}
			m.Capabilities = append(m.Capabilities, cap)
		}
	}
}

func (s *SpecDoc) buildFromV2(m *Manifest) {
	if s.v2.Paths == nil || s.v2.Paths.PathItems == nil {
		return
	}
	for path, item := range s.v2.Paths.PathItems.FromOldest() {
		ops := map[string]*v2high.Operation{
			http.MethodGet:     item.Get,
			http.MethodPut:     item.Put,
			http.MethodPost:    item.Post,
			http.MethodDelete:  item.Delete,
			http.MethodOptions: item.Options,
			http.MethodHead:    item.Head,
			http.MethodPatch:   item.Patch,
		}
		for method, op := range ops {
			if op == nil {
				continue
			}
			cap := Capability{
				Method:      method,
				Path:        path,
				OperationID: op.OperationId,
				Summary:     op.Summary,
				Access:      classifyMethod(method),
				Params:      v2Params(op.Parameters),
				Security:    v2Security(op.Security),
				Semantic:    classifySemantic(path, op.Tags, op.Summary),
				Provenance:  ProvenanceSpecOnly,
			}
			m.Capabilities = append(m.Capabilities, cap)
		}
	}
}

func v3Params(ps []*v3high.Parameter) []Param {
	out := make([]Param, 0, len(ps))
	for _, p := range ps {
		if p == nil {
			continue
		}
		req := false
		if p.Required != nil {
			req = *p.Required
		}
		out = append(out, Param{Name: p.Name, In: p.In, Required: req})
	}
	return out
}

func v2Params(ps []*v2high.Parameter) []Param {
	out := make([]Param, 0, len(ps))
	for _, p := range ps {
		if p == nil {
			continue
		}
		req := false
		if p.Required != nil {
			req = *p.Required
		}
		out = append(out, Param{Name: p.Name, In: p.In, Required: req})
	}
	return out
}

// v3Security and v2Security both read security requirements off the SAME
// datamodel/high/base.SecurityRequirement type (libopenapi shares it across v2/v3),
// so they delegate to one extractor.
func v3Security(reqs []*v3base.SecurityRequirement) []string { return securityNames(reqs) }
func v2Security(reqs []*v3base.SecurityRequirement) []string { return securityNames(reqs) }

func securityNames(reqs []*v3base.SecurityRequirement) []string {
	var out []string
	for _, r := range reqs {
		if r == nil || r.Requirements == nil {
			continue
		}
		for name := range r.Requirements.FromOldest() {
			out = append(out, name)
		}
	}
	return out
}

// deriveOrderTypes fills feature flags that are cheap to infer from the manifest
// itself: if there's an order surface, market+limit are assumed available until a
// richer schema walk refines them (a later slice). Asset classes/paper/websocket
// are tagged from path keywords.
func deriveOrderTypes(m *Manifest) {
	for _, c := range m.Capabilities {
		lp := strings.ToLower(c.Path)
		switch {
		case strings.Contains(lp, "crypto"):
			m.Features.AssetClasses["crypto"] = true
		case strings.Contains(lp, "option"):
			m.Features.AssetClasses["option"] = true
		case strings.Contains(lp, "equit") || strings.Contains(lp, "stock"):
			m.Features.AssetClasses["equity"] = true
		}
		if strings.Contains(lp, "sandbox") || strings.Contains(lp, "paper") {
			m.Features.PaperMode = true
		}
		if strings.Contains(lp, "stream") || strings.Contains(lp, "ws") || strings.Contains(lp, "websocket") {
			m.Features.WebSocket = true
		}
		if c.Semantic == SemanticOrders && c.Access == AccessWrite {
			m.Features.OrderTypes["market"] = true
			m.Features.OrderTypes["limit"] = true
		}
	}
}

func normalizeMethod(m string) string { return strings.ToUpper(m) }
