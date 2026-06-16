package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// methodLog records every HTTP method the test server received, so a test can
// assert that the probe NEVER issued a write (POST/PUT/PATCH/DELETE) while
// learning the API. This is the read-only guarantee proven against reality.
type methodLog struct {
	mu      sync.Mutex
	methods []string
}

func (l *methodLog) record(m string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.methods = append(l.methods, m)
}

func (l *methodLog) writes() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var w []string
	for _, m := range l.methods {
		switch m {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			w = append(w, m)
		}
	}
	return w
}

// fullSpec is a small but real OAS 3.0 spec with $refs (Account schema is a
// component referenced from the /account response), READ ops (GET account/quote)
// and a WRITE op (POST orders). It exercises parse + $ref resolution + READ/WRITE
// classification + semantic tagging.
const fullSpec = `{
  "openapi": "3.0.3",
  "info": {"title": "FakeVenue", "version": "1.0.0"},
  "paths": {
    "/v1/account": {
      "get": {
        "operationId": "getAccount",
        "summary": "Account balance",
        "tags": ["account"],
        "security": [{"bearerAuth": []}],
        "responses": {"200": {"description": "ok",
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Account"}}}}}
      }
    },
    "/v1/quotes/{symbol}": {
      "get": {
        "operationId": "getQuote",
        "summary": "Latest quote",
        "tags": ["marketdata"],
        "parameters": [{"name": "symbol", "in": "path", "required": true,
          "schema": {"type": "string"}}],
        "responses": {"200": {"description": "ok"}}
      }
    },
    "/v1/orders": {
      "post": {
        "operationId": "placeOrder",
        "summary": "Place an order",
        "tags": ["orders"],
        "security": [{"bearerAuth": []}],
        "responses": {"201": {"description": "created"}}
      }
    }
  },
  "components": {
    "schemas": {
      "Account": {
        "type": "object",
        "properties": {
          "cash": {"type": "string"},
          "equity": {"type": "string"}
        }
      }
    }
  }
}`

// newSpecServer builds an httptest server that serves the given spec at the given
// discovery path, answers OPTIONS with an Allow header, GET/HEAD for read paths,
// and records every method seen. Writes (POST orders) are accepted but their
// arrival is logged so a test can prove the probe never sent one.
func newSpecServer(t *testing.T, specPath, spec string) (*httptest.Server, *methodLog) {
	t.Helper()
	log := &methodLog{}
	mux := http.NewServeMux()

	// Serve the spec only at specPath; everything else 404s so discovery's
	// "absence is not an error, try the next" path is exercised.
	mux.HandleFunc(specPath, func(w http.ResponseWriter, r *http.Request) {
		log.record(r.Method)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(spec))
	})

	// READ surfaces: account + quotes respond to GET/HEAD/OPTIONS.
	mux.HandleFunc("/v1/account", func(w http.ResponseWriter, r *http.Request) {
		log.record(r.Method)
		if r.Method == http.MethodOptions {
			w.Header().Set("Allow", "GET, HEAD, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(`{"cash":"100.00","equity":"100.00"}`))
	})
	mux.HandleFunc("/v1/quotes", func(w http.ResponseWriter, r *http.Request) {
		log.record(r.Method)
		if r.Method == http.MethodOptions {
			w.Header().Set("Allow", "GET, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(`{"bid":"1","ask":"2"}`))
	})
	// WRITE surface: /v1/orders advertises POST via OPTIONS Allow, but the probe
	// must NEVER actually POST here.
	mux.HandleFunc("/v1/orders", func(w http.ResponseWriter, r *http.Request) {
		log.record(r.Method)
		if r.Method == http.MethodOptions {
			w.Header().Set("Allow", "POST, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})

	srv := httptest.NewServer(loggingNotFound(mux, log))
	t.Cleanup(srv.Close)
	return srv, log
}

// loggingNotFound wraps a mux so even 404s (unmatched discovery probes) are
// recorded — making the method log a complete record of every request.
func loggingNotFound(mux *http.ServeMux, log *methodLog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		if pattern == "" {
			log.record(r.Method)
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func mustRun(t *testing.T, srv *httptest.Server, mapper Mapper) *Result {
	t.Helper()
	p := NewProbe(srv.Client(), srv.URL, nil, mapper)
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("probe Run: %v", err)
	}
	return res
}

func TestDiscovery_WellKnown(t *testing.T) {
	srv, _ := newSpecServer(t, "/.well-known/api-catalog", fullSpec)
	ro := newROClient(srv.Client(), srv.URL, nil)
	spec, err := Discover(context.Background(), ro)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if spec.FoundAt != "/.well-known/api-catalog" {
		t.Fatalf("found at %q, want well-known", spec.FoundAt)
	}
}

func TestDiscovery_OpenAPIJSON(t *testing.T) {
	srv, _ := newSpecServer(t, "/openapi.json", fullSpec)
	ro := newROClient(srv.Client(), srv.URL, nil)
	spec, err := Discover(context.Background(), ro)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if spec.FoundAt != "/openapi.json" {
		t.Fatalf("found at %q, want /openapi.json", spec.FoundAt)
	}
}

func TestDiscovery_FrameworkFallback(t *testing.T) {
	// Only /v3/api-docs serves the spec; the earlier locations 404. Discovery
	// must skip the absent ones and find the fallback.
	srv, _ := newSpecServer(t, "/v3/api-docs", fullSpec)
	ro := newROClient(srv.Client(), srv.URL, nil)
	spec, err := Discover(context.Background(), ro)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if spec.FoundAt != "/v3/api-docs" {
		t.Fatalf("found at %q, want /v3/api-docs", spec.FoundAt)
	}
}

func TestDiscovery_AllAbsent_ClearNotFound(t *testing.T) {
	// Server that 404s everything — no spec anywhere.
	log := &methodLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r.Method)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	ro := newROClient(srv.Client(), srv.URL, nil)
	_, err := Discover(context.Background(), ro)
	if err != ErrSpecNotFound {
		t.Fatalf("got %v, want ErrSpecNotFound", err)
	}
}

func TestParse_RefsResolved_AndReadWriteClassified(t *testing.T) {
	srv, _ := newSpecServer(t, "/openapi.json", fullSpec)
	res := mustRun(t, srv, nil)
	m := res.Manifest

	if m.SpecVersion == "" {
		t.Fatalf("spec version not captured")
	}

	// READ vs WRITE classification.
	var getAccount, getQuote, postOrder *Capability
	for i := range m.Capabilities {
		switch {
		case m.Capabilities[i].Method == "GET" && m.Capabilities[i].Path == "/v1/account":
			getAccount = &m.Capabilities[i]
		case m.Capabilities[i].Method == "GET" && m.Capabilities[i].Path == "/v1/quotes/{symbol}":
			getQuote = &m.Capabilities[i]
		case m.Capabilities[i].Method == "POST" && m.Capabilities[i].Path == "/v1/orders":
			postOrder = &m.Capabilities[i]
		}
	}
	if getAccount == nil || getQuote == nil || postOrder == nil {
		t.Fatalf("missing capabilities: account=%v quote=%v order=%v", getAccount, getQuote, postOrder)
	}
	if getAccount.Access != AccessRead {
		t.Errorf("GET account = %v, want READ", getAccount.Access)
	}
	if postOrder.Access != AccessWrite {
		t.Errorf("POST orders = %v, want WRITE", postOrder.Access)
	}
	// Semantic tagging.
	if getAccount.Semantic != SemanticAccount {
		t.Errorf("account semantic = %v, want account", getAccount.Semantic)
	}
	if getQuote.Semantic != SemanticQuote {
		t.Errorf("quote semantic = %v, want quote", getQuote.Semantic)
	}
	if postOrder.Semantic != SemanticOrders {
		t.Errorf("order semantic = %v, want orders", postOrder.Semantic)
	}
	// $ref-resolved: the spec built without error (the Account $ref in the
	// /v1/account response inlined). The path parameter on the quote op survived.
	foundSymbolParam := false
	for _, p := range getQuote.Params {
		if p.Name == "symbol" && p.In == "path" && p.Required {
			foundSymbolParam = true
		}
	}
	if !foundSymbolParam {
		t.Errorf("quote op missing required path param 'symbol'; params=%+v", getQuote.Params)
	}
	// Security scheme captured from the $ref-bearing op.
	if len(getAccount.Security) == 0 || getAccount.Security[0] != "bearerAuth" {
		t.Errorf("account security = %v, want [bearerAuth]", getAccount.Security)
	}
}

func TestReadOnlyProbe_OptionsAllowEnumerates_AndNeverWrites(t *testing.T) {
	srv, log := newSpecServer(t, "/openapi.json", fullSpec)
	res := mustRun(t, srv, nil)

	// THE read-only guarantee: zero writes hit the server during the whole probe.
	if w := log.writes(); len(w) != 0 {
		t.Fatalf("probe issued write methods %v — read-only guarantee violated", w)
	}

	// OPTIONS Allow enumerated the read methods → those ops verified.
	for _, c := range res.Manifest.Capabilities {
		if c.Method == "GET" && c.Path == "/v1/account" {
			if c.Provenance != ProvenanceVerified {
				t.Errorf("account GET provenance = %v, want verified", c.Provenance)
			}
		}
	}
	// The WRITE op was enumerated by OPTIONS Allow (POST listed) so it is verified
	// as PRESENT — but it was verified WITHOUT the probe ever issuing a POST.
	for _, c := range res.Manifest.Capabilities {
		if c.Method == "POST" && c.Path == "/v1/orders" {
			if c.Provenance != ProvenanceVerified {
				t.Errorf("order POST provenance = %v, want verified-via-OPTIONS", c.Provenance)
			}
		}
	}
}

func TestReadOnlyClient_RefusesWriteMethods(t *testing.T) {
	srv, log := newSpecServer(t, "/openapi.json", fullSpec)
	ro := newROClient(srv.Client(), srv.URL, nil)
	// The read-only client must REFUSE non-read methods in code, before sending.
	if _, err := ro.do(context.Background(), http.MethodPost, "/v1/orders"); err == nil {
		t.Fatalf("read-only client allowed a POST")
	}
	if _, err := ro.do(context.Background(), http.MethodDelete, "/v1/orders"); err == nil {
		t.Fatalf("read-only client allowed a DELETE")
	}
	// And it sent nothing to the server.
	if w := log.writes(); len(w) != 0 {
		t.Fatalf("refused writes still reached the server: %v", w)
	}
}

func TestReadOnlyProbe_AbsentVsAuth(t *testing.T) {
	// Server: /openapi.json has the spec describing /missing (GET, absent live →
	// 404) and /secret (GET, exists but 401). Probe must mark absent as NOT
	// verified and auth as verified-present (endpoint exists).
	spec := `{
      "openapi":"3.0.3","info":{"title":"x","version":"1"},
      "paths":{
        "/missing":{"get":{"operationId":"m","responses":{"200":{"description":"ok"}}}},
        "/secret":{"get":{"operationId":"s","responses":{"200":{"description":"ok"}}}}
      }}`
	log := &methodLog{}
	mux := http.NewServeMux()
	mux.HandleFunc("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		log.record(r.Method)
		_, _ = w.Write([]byte(spec))
	})
	// /missing → 404 (and 405 on OPTIONS too) = ABSENT.
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		log.record(r.Method)
		http.Error(w, "nope", http.StatusNotFound)
	})
	// /secret → 401 on both OPTIONS and HEAD = AUTH (exists).
	mux.HandleFunc("/secret", func(w http.ResponseWriter, r *http.Request) {
		log.record(r.Method)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	srv := httptest.NewServer(loggingNotFound(mux, log))
	t.Cleanup(srv.Close)

	res := mustRun(t, srv, nil)
	if w := log.writes(); len(w) != 0 {
		t.Fatalf("probe wrote during absent/auth probe: %v", w)
	}
	var missing, secret *Capability
	for i := range res.Manifest.Capabilities {
		switch res.Manifest.Capabilities[i].Path {
		case "/missing":
			missing = &res.Manifest.Capabilities[i]
		case "/secret":
			secret = &res.Manifest.Capabilities[i]
		}
	}
	if missing == nil || secret == nil {
		t.Fatalf("missing caps: %+v", res.Manifest.Capabilities)
	}
	// 404/405 everywhere → feature absent → stays spec-only (NOT verified).
	if missing.Provenance == ProvenanceVerified {
		t.Errorf("/missing should NOT be verified (404 absent); got %v", missing.Provenance)
	}
	// 401 → endpoint exists, distinct from absent → verified-present.
	if secret.Provenance != ProvenanceVerified {
		t.Errorf("/secret should be verified-present (401 auth, exists); got %v", secret.Provenance)
	}
}

func TestDoubleGroundedMapping_DropsUnvalidated_KeepsValidated(t *testing.T) {
	srv, _ := newSpecServer(t, "/openapi.json", fullSpec)

	// The mapper proposes TWO mappings:
	//   (1) GET /v1/account as 'account'  — IS in the spec AND live → must verify.
	//   (2) GET /v1/phantom as 'positions'— NOT in the spec, NOT live → must drop.
	mapper := func(_ context.Context, _ *Manifest) []Mapping {
		return []Mapping{
			{Semantic: SemanticAccount, Method: "GET", Path: "/v1/account"},
			{Semantic: SemanticPositions, Method: "GET", Path: "/v1/phantom"},
		}
	}
	res := mustRun(t, srv, mapper)

	// The phantom mapping never enters the manifest.
	for _, c := range res.Manifest.Capabilities {
		if c.Path == "/v1/phantom" {
			t.Fatalf("unvalidated proposal /v1/phantom was promoted: %+v", c)
		}
	}
	// The validated mapping is present and verified.
	found := false
	for _, c := range res.Manifest.Capabilities {
		if c.Method == "GET" && c.Path == "/v1/account" {
			found = true
			if c.Provenance != ProvenanceVerified {
				t.Errorf("validated mapping /v1/account provenance = %v, want verified", c.Provenance)
			}
		}
	}
	if !found {
		t.Fatalf("validated mapping /v1/account not in manifest")
	}
}

func TestDoubleGroundedMapping_SpecOnlyButNotLive_Dropped(t *testing.T) {
	// A proposal that IS in the spec but is NOT confirmable live must NOT be
	// promoted to verified (it stays at whatever the live probe set). We use the
	// /missing path which is in the spec but 404s live.
	spec := `{
      "openapi":"3.0.3","info":{"title":"x","version":"1"},
      "paths":{"/missing":{"get":{"operationId":"m","responses":{"200":{"description":"ok"}}}}}}`
	log := &methodLog{}
	mux := http.NewServeMux()
	mux.HandleFunc("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		log.record(r.Method)
		_, _ = w.Write([]byte(spec))
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		log.record(r.Method)
		http.Error(w, "nope", http.StatusNotFound)
	})
	srv := httptest.NewServer(loggingNotFound(mux, log))
	t.Cleanup(srv.Close)

	mapper := func(_ context.Context, _ *Manifest) []Mapping {
		return []Mapping{{Semantic: SemanticAccount, Method: "GET", Path: "/missing"}}
	}
	res := mustRun(t, srv, mapper)
	for _, c := range res.Manifest.Capabilities {
		if c.Path == "/missing" && c.Provenance == ProvenanceVerified {
			t.Fatalf("/missing not confirmable live but was promoted to verified")
		}
	}
}

func TestWriteGate_LockedUntilAllThree_ReadsAlwaysAllowed(t *testing.T) {
	g := NewWriteGate()

	// Reads always allowed, from the very start.
	if err := g.AssertReadAllowed(); err != nil {
		t.Fatalf("reads must always be allowed: %v", err)
	}

	// Step 0: nothing done → read-probe required first.
	if err := g.AssertWriteAllowed(); err != ErrReadProbeRequired {
		t.Fatalf("step0 = %v, want ErrReadProbeRequired", err)
	}
	// Can't paper-run before read-probe.
	if err := g.RecordPaperRun(); err != ErrReadProbeRequired {
		t.Fatalf("paper-run before probe = %v, want ErrReadProbeRequired", err)
	}

	// Step 1: read-probe done → paper-run required.
	g.markReadProbed()
	if err := g.AssertWriteAllowed(); err != ErrPaperRunRequired {
		t.Fatalf("step1 = %v, want ErrPaperRunRequired", err)
	}
	if err := g.AssertReadAllowed(); err != nil {
		t.Fatalf("reads still allowed: %v", err)
	}

	// Step 2: paper-run done → operator-confirm required.
	if err := g.RecordPaperRun(); err != nil {
		t.Fatalf("paper-run after probe: %v", err)
	}
	if err := g.AssertWriteAllowed(); err != ErrOperatorConfirmRequired {
		t.Fatalf("step2 = %v, want ErrOperatorConfirmRequired", err)
	}

	// Operator denies → still locked.
	g.ConfirmWrites(false)
	if err := g.AssertWriteAllowed(); err != ErrOperatorConfirmRequired {
		t.Fatalf("operator denied but writes = %v, want still locked", err)
	}

	// Step 3: operator confirms → writes finally allowed.
	g.ConfirmWrites(true)
	if err := g.AssertWriteAllowed(); err != nil {
		t.Fatalf("all three done but writes blocked: %v", err)
	}
	// Reads were never blocked at any step.
	if err := g.AssertReadAllowed(); err != nil {
		t.Fatalf("reads must always be allowed: %v", err)
	}
}

// TestConcretePath_StripsFromFirstTemplate proves concretePath returns everything
// BEFORE the leftmost templated segment, so the live read-only probe gets a path it
// can actually match. The "mid-template" case is the bite: a path like
// /v1/orders/{id}/fills has a STATIC suffix after the template. The OLD logic only
// stripped a TRAILING template, so it left /v1/orders/{id}/fills unchanged (literal
// {id}) and the live probe could never match it — the capability stayed stuck
// spec-only. The new logic strips from the first '{' onward → /v1/orders. This
// table FAILS the mid-template + leading-template rows under the old break-on-
// trailing logic and PASSES under the fix.
func TestConcretePath_StripsFromFirstTemplate(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"no-template-unchanged", "/account", "/account"},
		{"no-template-deep", "/v1/markets/quotes", "/v1/markets/quotes"},
		{"trailing-template", "/positions/{symbol}", "/positions"},
		// MID template with a static suffix — the row that bites the old logic.
		{"mid-template-static-suffix", "/v1/orders/{id}/fills", "/v1/orders"},
		{"mid-template-deeper-suffix", "/v1/orders/{id}/fills/{fillId}/detail", "/v1/orders"},
		{"multiple-templates", "/v1/accounts/{acct}/orders/{id}", "/v1/accounts"},
		{"leading-template", "/{tenant}/orders", "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := concretePath(tc.path); got != tc.want {
				t.Errorf("concretePath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestWriteGate_Blocks2of3 proves NO 2-of-3 partial state ever leaks a write. The
// gate requires the FULL conjunction (read-probe AND paper-run AND operator-
// confirm). We exercise both adversarial 2-of-3 holes:
//
//	(A) read-probe + operator-confirm, but NO paper-run → ErrPaperRunRequired.
//	(B) paper-run + operator-confirm, but read-probe FALSE → ErrReadProbeRequired
//	    (and the gate must refuse to record a paper-run at all without the probe,
//	    so this state can only be reached via the probe path — which is exactly
//	    what we assert: you cannot get paper-run set with readProbed false).
//
// Neither 2-of-3 combination is allowed to return nil.
func TestWriteGate_Blocks2of3(t *testing.T) {
	// (A) read-probe + operator-confirm set, paper-run MISSING → blocked.
	t.Run("read+confirm_no_paper", func(t *testing.T) {
		g := NewWriteGate()
		g.markReadProbed()    // condition 1 satisfied via the probe path.
		g.ConfirmWrites(true) // condition 3 satisfied (operator approved).
		// DELIBERATELY do NOT call RecordPaperRun — condition 2 is missing.
		if err := g.AssertWriteAllowed(); err != ErrPaperRunRequired {
			t.Fatalf("2-of-3 (read+confirm, no paper) = %v, want ErrPaperRunRequired", err)
		}
	})

	// (B) operator-confirm + (attempted) paper-run, but read-probe FALSE → blocked.
	// The gate fail-SAFE design means RecordPaperRun itself refuses without the
	// read-probe, so paper-run can NEVER be set while readProbed is false. We assert
	// both: RecordPaperRun is refused, AND a write stays blocked on ErrReadProbe.
	t.Run("confirm+attempted_paper_no_probe", func(t *testing.T) {
		g := NewWriteGate()
		g.ConfirmWrites(true) // operator approved...
		// Try to satisfy paper-run WITHOUT the read-probe — must be refused.
		if err := g.RecordPaperRun(); err != ErrReadProbeRequired {
			t.Fatalf("paper-run without read-probe = %v, want ErrReadProbeRequired", err)
		}
		if g.PaperRunPassed() {
			t.Fatalf("paper-run leaked true without a read-probe")
		}
		// So with read-probe false, a write is blocked at the read-probe step even
		// though operator-confirm is set — no 2-of-3 leak.
		if err := g.AssertWriteAllowed(); err != ErrReadProbeRequired {
			t.Fatalf("2-of-3 (confirm+paper-attempt, no probe) = %v, want ErrReadProbeRequired", err)
		}
	})
}

func TestRun_GateStartsReadProbedButLocked(t *testing.T) {
	srv, _ := newSpecServer(t, "/openapi.json", fullSpec)
	res := mustRun(t, srv, nil)
	// After Run, the read-probe has happened...
	if !res.Gate.ReadProbed() {
		t.Fatalf("gate should record read-probe after Run")
	}
	// ...but writes are still locked (paper-run + operator-confirm outstanding).
	if err := res.Gate.AssertWriteAllowed(); err != ErrPaperRunRequired {
		t.Fatalf("after Run writes = %v, want ErrPaperRunRequired", err)
	}
}
