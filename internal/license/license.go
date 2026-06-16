// Package license is BUCKS's ship-time license gate. BUCKS ships MIT, and the
// operator's law is hard: an (A)GPL/LGPL dependency anywhere in the tree would
// force the whole app open (AGPL's network clause is the sharp one), so the build
// must HARD-FAIL the instant a copyleft — or an unknown, un-vetted — license shows
// up in what we link. This package is that gate.
//
// DESIGN (no band-aid): we do NOT guess a license from a string heuristic at scan
// time. We carry a CURATED, human-reviewed map of module -> SPDX id (KnownLicenses)
// for every dependency BUCKS actually links — each entry was read off the module's
// real LICENSE file (file:header verified at build time). The scanner's only job is
// to classify each module's SPDX id into ALLOW / DENY / UNKNOWN and fail on anything
// that is not explicitly allowed. A new dependency with no curated entry is UNKNOWN
// and FAILS — you cannot accidentally ship an unreviewed license. That is the safe
// default: unknown is a failure, never a silent pass.
//
// This is OUR gate, modeled on the idea of a license scanner but implemented natively
// for BUCKS's exact dependency surface — not a vendored tool, not brand-named.
package license

import (
	"fmt"
	"sort"
	"strings"
)

// Classification is how the gate treats a given SPDX license id.
type Classification int

const (
	// ClassUnknown is a license id we have NOT curated/reviewed. It FAILS the gate
	// (the safe default — an unreviewed license is never silently shipped).
	ClassUnknown Classification = iota
	// ClassAllowed is a permissive license compatible with shipping BUCKS MIT
	// (MIT, BSD-2/3, ISC, Apache-2.0, the Go BSD-style, public domain).
	ClassAllowed
	// ClassCopyleft is a (A)GPL/LGPL/MPL-family license that would impose source
	// obligations on BUCKS. It FAILS the gate — the operator's hard line.
	ClassCopyleft
)

// String renders a classification for reports and test failures.
func (c Classification) String() string {
	switch c {
	case ClassAllowed:
		return "allowed"
	case ClassCopyleft:
		return "copyleft"
	default:
		return "unknown"
	}
}

// allowedSPDX is the set of permissive SPDX ids that are safe to ship under MIT.
// Apache-2.0 is permissive (patent grant + attribution) and MIT-compatible for a
// shipped binary, so it is allowed; its NOTICE obligations are met by our NOTICE
// file. Anything not in this set and not in deniedSPDX is UNKNOWN -> fails.
var allowedSPDX = map[string]bool{
	"MIT":          true,
	"BSD-2-Clause": true,
	"BSD-3-Clause": true,
	"ISC":          true,
	"Apache-2.0":   true,
	"Unlicense":    true, // public-domain dedication
	"CC0-1.0":      true, // public-domain dedication
}

// deniedSPDX is the set of copyleft SPDX families that HARD-FAIL the gate. The
// network-copyleft AGPL is the headline, but every GPL/LGPL and the weak-copyleft
// MPL/EPL/CDDL are denied too — BUCKS ships permissive or it does not ship.
var deniedSPDX = map[string]bool{
	"AGPL-1.0":          true,
	"AGPL-3.0":          true,
	"AGPL-3.0-only":     true,
	"AGPL-3.0-or-later": true,
	"GPL-2.0":           true,
	"GPL-2.0-only":      true,
	"GPL-2.0-or-later":  true,
	"GPL-3.0":           true,
	"GPL-3.0-only":      true,
	"GPL-3.0-or-later":  true,
	"LGPL-2.1":          true,
	"LGPL-2.1-only":     true,
	"LGPL-3.0":          true,
	"LGPL-3.0-only":     true,
	"MPL-1.1":           true,
	"MPL-2.0":           true,
	"EPL-1.0":           true,
	"EPL-2.0":           true,
	"CDDL-1.0":          true,
	"SSPL-1.0":          true, // server-side-public-license (network copyleft)
}

// Classify maps an SPDX id to its gate Classification. A copyleft id is denied; an
// id in the allow-list is allowed; ANYTHING else is unknown (and thus fails). The
// id is compared case-insensitively but otherwise exactly — we never fuzzy-match a
// license name into "probably fine".
func Classify(spdx string) Classification {
	id := canonical(spdx)
	if id == "" {
		return ClassUnknown
	}
	if deniedSPDX[id] {
		return ClassCopyleft
	}
	if allowedSPDX[id] {
		return ClassAllowed
	}
	return ClassUnknown
}

// canonical normalizes an SPDX id for lookup: trims spaces and matches our map keys
// case-insensitively by upper-casing and re-resolving against the known keys. We do
// NOT alter the structure of the id (no synonym expansion) — just case/space.
func canonical(spdx string) string {
	s := strings.TrimSpace(spdx)
	if s == "" {
		return ""
	}
	up := strings.ToUpper(s)
	for k := range deniedSPDX {
		if strings.ToUpper(k) == up {
			return k
		}
	}
	for k := range allowedSPDX {
		if strings.ToUpper(k) == up {
			return k
		}
	}
	// Return the trimmed-but-otherwise-original id so an unknown is reported as the
	// caller wrote it (helps the operator identify the offending license).
	return s
}

// Finding is the per-module result of a scan: the module path, the SPDX id we have
// on record (empty when the module is not in the curated map), and the gate verdict.
type Finding struct {
	Module string
	SPDX   string // "" when the module has no curated entry (-> Unknown)
	Class  Classification
}

// OK reports whether this finding passes the gate (only an allowed license passes).
func (f Finding) OK() bool { return f.Class == ClassAllowed }

// Report is the full scan result over a set of modules.
type Report struct {
	Findings []Finding // one per scanned module, sorted by module path
	Allowed  int
	Denied   []Finding // copyleft hits (the AGPL/GPL/etc. offenders)
	Unknown  []Finding // modules with no curated, reviewed license
}

// Passed reports whether the whole tree is clean: zero denied AND zero unknown.
func (r Report) Passed() bool { return len(r.Denied) == 0 && len(r.Unknown) == 0 }

// Summary renders a one-line plain-English verdict for logs and the CI gate.
func (r Report) Summary() string {
	if r.Passed() {
		return fmt.Sprintf("license scan PASS: %d modules, all permissive (MIT/BSD/ISC/Apache-2.0)", len(r.Findings))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "license scan FAIL: %d copyleft, %d unknown", len(r.Denied), len(r.Unknown))
	if len(r.Denied) > 0 {
		names := make([]string, 0, len(r.Denied))
		for _, f := range r.Denied {
			names = append(names, fmt.Sprintf("%s (%s)", f.Module, f.SPDX))
		}
		fmt.Fprintf(&b, " — copyleft: %s", strings.Join(names, ", "))
	}
	if len(r.Unknown) > 0 {
		names := make([]string, 0, len(r.Unknown))
		for _, f := range r.Unknown {
			names = append(names, f.Module)
		}
		fmt.Fprintf(&b, " — unknown (add to curated map after review): %s", strings.Join(names, ", "))
	}
	return b.String()
}

// Scan classifies every module in `modules` (a module-path -> SPDX-id map) against
// the gate and returns a Report. It is the heart of the CI gate. It returns a
// non-nil error IFF the tree fails (copyleft or unknown present) so a caller can
// `if _, err := Scan(...); err != nil { fail }` directly; the Report is always
// returned too (even on failure) for detailed reporting.
//
// The error names the offending module(s) — the operator/CI sees exactly what to
// fix, never a bare "scan failed".
func Scan(modules map[string]string) (Report, error) {
	var rep Report
	rep.Findings = make([]Finding, 0, len(modules))
	for mod, spdx := range modules {
		f := Finding{Module: mod, SPDX: spdx, Class: Classify(spdx)}
		// An empty SPDX (module absent from the curated map upstream) is unknown
		// regardless of Classify (Classify("") is already ClassUnknown).
		rep.Findings = append(rep.Findings, f)
		switch f.Class {
		case ClassAllowed:
			rep.Allowed++
		case ClassCopyleft:
			rep.Denied = append(rep.Denied, f)
		default:
			rep.Unknown = append(rep.Unknown, f)
		}
	}
	sort.Slice(rep.Findings, func(i, j int) bool { return rep.Findings[i].Module < rep.Findings[j].Module })
	sort.Slice(rep.Denied, func(i, j int) bool { return rep.Denied[i].Module < rep.Denied[j].Module })
	sort.Slice(rep.Unknown, func(i, j int) bool { return rep.Unknown[i].Module < rep.Unknown[j].Module })

	if !rep.Passed() {
		return rep, fmt.Errorf("license: %s", rep.Summary())
	}
	return rep, nil
}

// ScanCurated scans BUCKS's OWN curated dependency map (KnownLicenses). This is the
// CI gate the build runs: it proves the real, shipped tree is 100% permissive. A
// future dependency added without a curated entry will not appear here until it is
// reviewed and added — and the cross-check test (every go.mod require has a curated
// entry) is what forces that review.
func ScanCurated() (Report, error) {
	return Scan(KnownLicenses)
}
