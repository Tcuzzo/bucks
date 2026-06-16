// Package orders is the BUCKS order durability spine: the core that guarantees
// the trader never loses or duplicates an order across a crash. It holds three
// things — a deterministic client-order-ID, an explicit order state machine with
// cumulative-fill accounting, and an append-only write-ahead journal (WAL).
package orders

import "github.com/govalues/decimal"

// Decimal is BUCKS's money type for all quantity, price, and PnL math.
//
// It is a named alias over github.com/govalues/decimal (zero-alloc, 19-digit
// fixed-point — never float64). The whole codebase refers to orders.Decimal, so
// the underlying implementation can be swapped (e.g. for an internal fixed-point
// type, or a Rust hot-core value) without touching call sites.
//
// NEVER use float64 anywhere in qty/px/PnL math: binary floats cannot represent
// most decimal money values exactly and accumulate drift across fills.
type Decimal = decimal.Decimal

// ParseDecimal parses a decimal string (e.g. "100.5", "0.00000001") into the
// BUCKS money type. It returns an error on malformed input rather than panicking.
func ParseDecimal(s string) (Decimal, error) {
	return decimal.Parse(s)
}

// MustParseDecimal is ParseDecimal that panics on error. Use only with constant,
// known-good literals (tests, fixed defaults) — never on external input.
func MustParseDecimal(s string) Decimal {
	return decimal.MustParse(s)
}

// NewDecimal builds a Decimal from an unscaled integer and a scale, where the
// value is coef * 10^-scale (e.g. NewDecimal(12345, 2) == 123.45). Scale must be
// in [0, 19]; an out-of-range scale returns an error.
func NewDecimal(coef int64, scale int) (Decimal, error) {
	return decimal.New(coef, scale)
}

// ZeroDecimal is the additive identity (0). The zero value of Decimal is already
// numerically zero; this is provided for readable intent at call sites.
var ZeroDecimal = decimal.Decimal{}
