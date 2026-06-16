package probe

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrWriteAttempted is returned by the read-only client if any caller asks it to
// issue a non-read HTTP method. It exists so the read-only guarantee is enforced
// in CODE: the probe cannot place, cancel, or mutate an order even by mistake —
// the only methods the client will physically send are GET, HEAD, and OPTIONS.
var ErrWriteAttempted = errors.New("probe: read-only client refused a non-read HTTP method")

// readMethods is the closed allow-list of HTTP methods the probe may issue. Any
// method not in this set is refused before a request is ever built — fail SAFE.
var readMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodOptions: true,
}

// roClient is an HTTP client that is PHYSICALLY incapable of issuing a write. It
// is the safety core of the capability probe: discovery, the live read-only
// probe, and the double-grounding step all go through it, so no probe code path
// can ever reach a state-changing endpoint. The only escape hatch is a separate,
// explicitly gated write path (see writegate.go) that this client never touches.
type roClient struct {
	http    *http.Client
	baseURL string
	// auth is an optional hook to attach credentials (e.g. a bearer token) to
	// each outbound request. Reads may legitimately require auth; attaching it
	// here keeps the read-only guarantee while still letting 401/403 be probed.
	auth func(*http.Request)
}

// newROClient builds a read-only client against baseURL. baseURL is trimmed of a
// trailing slash so callers pass paths like "/openapi.json".
func newROClient(hc *http.Client, baseURL string, auth func(*http.Request)) *roClient {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &roClient{
		http:    hc,
		baseURL: strings.TrimRight(baseURL, "/"),
		auth:    auth,
	}
}

// do issues a single read-only request. It REFUSES any method that is not
// GET/HEAD/OPTIONS — returning ErrWriteAttempted without building or sending a
// request — so the read-only guarantee holds even against a caller bug. The
// caller owns closing resp.Body.
func (c *roClient) do(ctx context.Context, method, path string) (*http.Response, error) {
	up := strings.ToUpper(method)
	if !readMethods[up] {
		return nil, fmt.Errorf("%w: %s", ErrWriteAttempted, up)
	}
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, up, url, nil)
	if err != nil {
		return nil, fmt.Errorf("probe: build %s %s: %w", up, path, err)
	}
	if c.auth != nil {
		c.auth(req)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("probe: %s %s: %w", up, path, err)
	}
	return resp, nil
}

// get issues a GET and returns status, body and headers, closing the body.
func (c *roClient) get(ctx context.Context, path string) (status int, body []byte, hdr http.Header, err error) {
	return c.readBody(ctx, http.MethodGet, path)
}

// head issues a HEAD (status + headers, no body).
func (c *roClient) head(ctx context.Context, path string) (status int, hdr http.Header, err error) {
	resp, e := c.do(ctx, http.MethodHead, path)
	if e != nil {
		return 0, nil, e
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header, nil
}

// options issues an OPTIONS and returns the methods enumerated by the Allow
// header (uppercased, de-duplicated) plus the status code. This lets the probe
// learn which methods a path supports WITHOUT invoking any of them.
func (c *roClient) options(ctx context.Context, path string) (status int, allow []string, err error) {
	resp, e := c.do(ctx, http.MethodOptions, path)
	if e != nil {
		return 0, nil, e
	}
	defer resp.Body.Close()
	return resp.StatusCode, parseAllow(resp.Header.Get("Allow")), nil
}

// readBody runs a GET/HEAD and reads (and closes) the body. Bounded by a 1 MiB
// cap so a hostile/huge response can't exhaust memory during probing.
func (c *roClient) readBody(ctx context.Context, method, path string) (int, []byte, http.Header, error) {
	resp, err := c.do(ctx, method, path)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	const maxBody = 1 << 20
	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 4096)
	for len(buf) < maxBody {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if rerr != nil {
			break
		}
	}
	return resp.StatusCode, buf, resp.Header, nil
}

// parseAllow parses an Allow header value ("GET, POST, OPTIONS") into a
// de-duplicated, uppercased method list.
func parseAllow(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(v, ",") {
		m := strings.ToUpper(strings.TrimSpace(part))
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// statusClass classifies a probe response status for the manifest: absent (the
// feature does not exist), auth (it exists but credentials are wrong/missing), or
// present (the surface is real). This is the 404/405 vs 401/403 distinction the
// spec calls out — "feature absent" must never be confused with "auth problem".
type statusClass int

const (
	statusPresent statusClass = iota // 2xx/3xx and other non-absent, non-auth codes
	statusAbsent                     // 404 / 405 — feature not present
	statusAuth                       // 401 / 403 — exists but auth failed
)

// classifyStatus maps an HTTP status code to a probe status class.
func classifyStatus(code int) statusClass {
	switch code {
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return statusAbsent
	case http.StatusUnauthorized, http.StatusForbidden:
		return statusAuth
	default:
		return statusPresent
	}
}
