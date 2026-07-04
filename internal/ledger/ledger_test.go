package ledger

import (
	"testing"

	"bucks/internal/orders"
)

func d(t *testing.T, s string) orders.Decimal {
	t.Helper()
	v, err := orders.ParseDecimal(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// realized applies one fill and returns the realized PnL, failing the test on error.
func realized(t *testing.T, a *Accountant, sym string, side orders.Side, qty, px orders.Decimal) orders.Decimal {
	t.Helper()
	r, err := a.Apply(sym, side, qty, px)
	if err != nil {
		t.Fatalf("apply %s %v %s@%s: %v", sym, side, qty, px, err)
	}
	return r
}

func eq(t *testing.T, got orders.Decimal, want string) {
	t.Helper()
	w := d(t, want)
	if got.Cmp(w) != 0 {
		t.Fatalf("got %s, want %s", got.String(), w.String())
	}
}

func TestOpeningLongRealizesNothing(t *testing.T) {
	a := New()
	eq(t, realized(t, a, "AAPL", orders.SideBuy, d(t, "10"), d(t, "100")), "0")
}

func TestCloseLongProfit(t *testing.T) {
	a := New()
	realized(t, a, "AAPL", orders.SideBuy, d(t, "10"), d(t, "100"))
	// sell 10 @ 110 -> realized 10 * (110-100) = 100
	eq(t, realized(t, a, "AAPL", orders.SideSell, d(t, "10"), d(t, "110")), "100")
}

func TestCloseLongLoss(t *testing.T) {
	a := New()
	realized(t, a, "AAPL", orders.SideBuy, d(t, "10"), d(t, "100"))
	// sell 10 @ 92 -> realized 10 * (92-100) = -80
	eq(t, realized(t, a, "AAPL", orders.SideSell, d(t, "10"), d(t, "92")), "-80")
}

func TestPartialCloseRealizesOnlyClosedQty(t *testing.T) {
	a := New()
	realized(t, a, "AAPL", orders.SideBuy, d(t, "10"), d(t, "100"))
	// sell 4 @ 110 -> realized 4*10 = 40; 6 still open
	eq(t, realized(t, a, "AAPL", orders.SideSell, d(t, "4"), d(t, "110")), "40")
	// close the rest 6 @ 105 -> 6*5 = 30
	eq(t, realized(t, a, "AAPL", orders.SideSell, d(t, "6"), d(t, "105")), "30")
}

func TestFifoLotOrdering(t *testing.T) {
	a := New()
	realized(t, a, "AAPL", orders.SideBuy, d(t, "10"), d(t, "100"))
	realized(t, a, "AAPL", orders.SideBuy, d(t, "10"), d(t, "120"))
	// sell 15 @ 130 -> FIFO: 10@100 (10*30=300) + 5@120 (5*10=50) = 350
	eq(t, realized(t, a, "AAPL", orders.SideSell, d(t, "15"), d(t, "130")), "350")
}

func TestShortRoundTrip(t *testing.T) {
	a := New()
	realized(t, a, "AAPL", orders.SideSell, d(t, "10"), d(t, "100")) // open short
	// buy 10 @ 90 -> realized 10*(100-90) = 100
	eq(t, realized(t, a, "AAPL", orders.SideBuy, d(t, "10"), d(t, "90")), "100")
}

func TestFlipLongToShort(t *testing.T) {
	a := New()
	realized(t, a, "AAPL", orders.SideBuy, d(t, "10"), d(t, "100"))
	// sell 15 @ 110 -> close 10 (realized 100), open short 5 @ 110 (realized 0 on the opener)
	eq(t, realized(t, a, "AAPL", orders.SideSell, d(t, "15"), d(t, "110")), "100")
	// buy 5 @ 105 closes the short -> 5*(110-105) = 25
	eq(t, realized(t, a, "AAPL", orders.SideBuy, d(t, "5"), d(t, "105")), "25")
}

func TestSeedFromBrokerPosition(t *testing.T) {
	a := New()
	// reconcile-on-boot: broker says we hold +10 AAPL avg 100.
	a.Seed("AAPL", d(t, "10"), d(t, "100"))
	// selling 10 @ 108 must realize against the seeded basis: 10*8 = 80
	eq(t, realized(t, a, "AAPL", orders.SideSell, d(t, "10"), d(t, "108")), "80")
}

func TestSeedShortPosition(t *testing.T) {
	a := New()
	a.Seed("AAPL", d(t, "-10"), d(t, "100")) // short 10 @ 100
	// buy 10 @ 95 -> 10*(100-95) = 50
	eq(t, realized(t, a, "AAPL", orders.SideBuy, d(t, "10"), d(t, "95")), "50")
}

func TestUnrelatedSymbolsAreIndependent(t *testing.T) {
	a := New()
	realized(t, a, "AAPL", orders.SideBuy, d(t, "10"), d(t, "100"))
	realized(t, a, "MSFT", orders.SideBuy, d(t, "5"), d(t, "200"))
	eq(t, realized(t, a, "MSFT", orders.SideSell, d(t, "5"), d(t, "210")), "50")
	eq(t, realized(t, a, "AAPL", orders.SideSell, d(t, "10"), d(t, "101")), "10")
}
