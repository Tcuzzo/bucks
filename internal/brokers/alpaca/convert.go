package alpaca

import (
	"fmt"

	shopspring "github.com/shopspring/decimal"

	"bucks/internal/orders"
)

// The Alpaca SDK models money with github.com/shopspring/decimal, while BUCKS
// uses orders.Decimal (github.com/govalues/decimal). These are DIFFERENT types,
// so every value crossing the SDK boundary is converted here — and ALWAYS via
// the exact decimal STRING, never via float64. Routing money through a binary
// float would reintroduce exactly the drift orders.Decimal exists to prevent.

// fromSDKDecimal converts a shopspring decimal (from the SDK) into orders.Decimal
// by exact decimal text. The zero value of shopspring.Decimal stringifies to "0"
// and parses back to numeric zero, so empty/zero fields convert cleanly.
func fromSDKDecimal(d shopspring.Decimal) (orders.Decimal, error) {
	v, err := orders.ParseDecimal(d.String())
	if err != nil {
		return orders.ZeroDecimal, fmt.Errorf("alpaca: convert %q from SDK decimal: %w", d.String(), err)
	}
	return v, nil
}

// toSDKDecimal converts an orders.Decimal into the SDK's shopspring decimal by
// exact decimal text (no float64).
func toSDKDecimal(d orders.Decimal) (shopspring.Decimal, error) {
	v, err := shopspring.NewFromString(d.String())
	if err != nil {
		return shopspring.Decimal{}, fmt.Errorf("alpaca: convert %q to SDK decimal: %w", d.String(), err)
	}
	return v, nil
}
