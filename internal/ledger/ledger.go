// Package ledger is BUCKS's realized-P&L accountant: it turns a stream of fills
// into exact realized profit-and-loss, so the daily-loss circuit breaker sees a
// REAL number instead of a hardcoded zero.
//
// Accounting is FIFO on per-symbol lots. Every realized figure is exact decimal
// (qty*(exit-entry), only Mul/Sub/Add — NEVER a division, so no rounding creeps
// into money). A fill that opens or extends a position realizes nothing; a fill
// that reduces, closes, or flips one realizes P&L against the oldest open lots
// first. Positions are seeded on boot from the broker's own view (the broker is
// the source of truth for what is held), so a restart does not lose cost basis.
package ledger

import (
	"encoding/json"
	"fmt"
	"sort"

	"bucks/internal/orders"
)

// Decimal is BUCKS's exact money type (never float64).
type Decimal = orders.Decimal

// lot is one open parcel of a position: a POSITIVE quantity entered at px. The
// parcel's direction is carried by the owning position, not the lot.
type lot struct {
	qty Decimal // strictly positive
	px  Decimal
}

// position is a symbol's open exposure: a direction and the FIFO queue of lots
// that make it up. dir is +1 (long), -1 (short), or 0 (flat, lots empty).
type position struct {
	dir  int
	lots []lot
}

type basisState struct {
	Version   int             `json:"version"`
	Positions []basisPosition `json:"positions"`
}

type basisPosition struct {
	Symbol string     `json:"symbol"`
	Dir    int        `json:"dir"`
	Lots   []basisLot `json:"lots"`
}

type basisLot struct {
	Qty string `json:"qty"`
	Px  string `json:"px"`
}

// Accountant computes realized P&L from fills using FIFO lot matching. It holds
// only open-position state (cost basis); realized figures are returned to the
// caller to persist. It is NOT safe for concurrent use — the trade loop applies
// fills serially. The zero value is not usable; call New.
type Accountant struct {
	pos map[string]*position
}

// New returns an empty Accountant with no open positions.
func New() *Accountant {
	return &Accountant{pos: make(map[string]*position)}
}

// Seed initializes a symbol's open position from an external source of truth
// (reconcile-on-boot: the broker's current position). signedQty is + for a long,
// - for a short, 0 for flat; avgPx is the average entry. Seeding replaces any
// existing state for the symbol, so it is idempotent per boot. A single lot is
// created at the average entry — realized P&L on the NEXT close is measured
// against that basis, which is what the broker itself reports.
func (a *Accountant) Seed(symbol string, signedQty, avgPx Decimal) {
	switch s := signedQty.Sign(); {
	case s == 0:
		delete(a.pos, symbol)
	default:
		a.pos[symbol] = &position{dir: s, lots: []lot{{qty: signedQty.Abs(), px: avgPx}}}
	}
}

func (a *Accountant) snapshot(symbol string) *position {
	if a == nil || a.pos == nil {
		return nil
	}
	return clonePosition(a.pos[symbol])
}

func (a *Accountant) restore(symbol string, prev *position) {
	if a.pos == nil {
		a.pos = make(map[string]*position)
	}
	if prev == nil {
		delete(a.pos, symbol)
		return
	}
	a.pos[symbol] = clonePosition(prev)
}

func clonePosition(p *position) *position {
	if p == nil {
		return nil
	}
	cp := &position{dir: p.dir}
	if len(p.lots) > 0 {
		cp.lots = make([]lot, len(p.lots))
		copy(cp.lots, p.lots)
	}
	return cp
}

func (a *Accountant) encodeBasis() ([]byte, error) {
	state := basisState{Version: 1}
	if a != nil {
		symbols := make([]string, 0, len(a.pos))
		for symbol, p := range a.pos {
			if p == nil || p.dir == 0 || len(p.lots) == 0 {
				continue
			}
			symbols = append(symbols, symbol)
		}
		sort.Strings(symbols)
		state.Positions = make([]basisPosition, 0, len(symbols))
		for _, symbol := range symbols {
			p := a.pos[symbol]
			bp := basisPosition{Symbol: symbol, Dir: p.dir, Lots: make([]basisLot, 0, len(p.lots))}
			for _, l := range p.lots {
				bp.Lots = append(bp.Lots, basisLot{Qty: l.qty.String(), Px: l.px.String()})
			}
			state.Positions = append(state.Positions, bp)
		}
	}
	data, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("ledger: encode basis: %w", err)
	}
	return data, nil
}

func (a *Accountant) restoreBasis(data []byte) error {
	if len(data) == 0 {
		a.pos = make(map[string]*position)
		return nil
	}
	var state basisState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("ledger: decode basis: %w", err)
	}
	if state.Version != 1 {
		return fmt.Errorf("ledger: unsupported basis version %d", state.Version)
	}
	next := make(map[string]*position, len(state.Positions))
	for _, bp := range state.Positions {
		if bp.Symbol == "" {
			return fmt.Errorf("ledger: basis position missing symbol")
		}
		if bp.Dir != 1 && bp.Dir != -1 {
			return fmt.Errorf("ledger: basis position %s has invalid dir %d", bp.Symbol, bp.Dir)
		}
		if len(bp.Lots) == 0 {
			return fmt.Errorf("ledger: basis position %s has no lots", bp.Symbol)
		}
		p := &position{dir: bp.Dir, lots: make([]lot, 0, len(bp.Lots))}
		for _, bl := range bp.Lots {
			qty, err := orders.ParseDecimal(bl.Qty)
			if err != nil {
				return fmt.Errorf("ledger: basis %s qty %q: %w", bp.Symbol, bl.Qty, err)
			}
			if qty.Sign() <= 0 {
				return fmt.Errorf("ledger: basis %s qty must be positive, got %s", bp.Symbol, qty.String())
			}
			px, err := orders.ParseDecimal(bl.Px)
			if err != nil {
				return fmt.Errorf("ledger: basis %s px %q: %w", bp.Symbol, bl.Px, err)
			}
			if px.Sign() <= 0 {
				return fmt.Errorf("ledger: basis %s px must be positive, got %s", bp.Symbol, px.String())
			}
			p.lots = append(p.lots, lot{qty: qty, px: px})
		}
		next[bp.Symbol] = p
	}
	a.pos = next
	return nil
}

// Apply ingests one fill and returns the realized P&L it produced. A fill that
// opens or extends a position realizes 0; a fill that reduces/closes/flips one
// realizes exact P&L against the oldest lots first. qty must be positive and px
// positive.
func (a *Accountant) Apply(symbol string, side orders.Side, qty, px Decimal) (Decimal, error) {
	if qty.Sign() <= 0 {
		return orders.ZeroDecimal, fmt.Errorf("ledger: fill qty must be positive, got %s", qty.String())
	}
	if px.Sign() <= 0 {
		return orders.ZeroDecimal, fmt.Errorf("ledger: fill price must be positive, got %s", px.String())
	}
	fillDir := 1
	if side == orders.SideSell {
		fillDir = -1
	}

	p := a.pos[symbol]
	if p == nil || p.dir == 0 {
		// Flat -> open in the fill's direction.
		a.pos[symbol] = &position{dir: fillDir, lots: []lot{{qty: qty, px: px}}}
		return orders.ZeroDecimal, nil
	}
	if p.dir == fillDir {
		// Same direction -> extend, realizes nothing.
		p.lots = append(p.lots, lot{qty: qty, px: px})
		return orders.ZeroDecimal, nil
	}

	// Opposite direction -> reduce/close/flip. Consume FIFO lots.
	remaining := qty
	realized := orders.ZeroDecimal
	for remaining.Sign() > 0 && len(p.lots) > 0 {
		l := &p.lots[0]
		take := l.qty
		if take.Cmp(remaining) > 0 {
			take = remaining
		}
		// Realized on `take` units. Long closed by a sell: (px-entry); short
		// closed by a buy: (entry-px). p.dir carries the sign exactly.
		var diff Decimal
		var err error
		if p.dir > 0 {
			diff, err = px.Sub(l.px)
		} else {
			diff, err = l.px.Sub(px)
		}
		if err != nil {
			return orders.ZeroDecimal, fmt.Errorf("ledger: pnl diff: %w", err)
		}
		gain, err := take.Mul(diff)
		if err != nil {
			return orders.ZeroDecimal, fmt.Errorf("ledger: pnl mul: %w", err)
		}
		realized, err = realized.Add(gain)
		if err != nil {
			return orders.ZeroDecimal, fmt.Errorf("ledger: pnl add: %w", err)
		}

		newLotQty, err := l.qty.Sub(take)
		if err != nil {
			return orders.ZeroDecimal, fmt.Errorf("ledger: lot reduce: %w", err)
		}
		if newLotQty.Sign() == 0 {
			p.lots = p.lots[1:] // fully consumed
		} else {
			l.qty = newLotQty
		}
		remaining, err = remaining.Sub(take)
		if err != nil {
			return orders.ZeroDecimal, fmt.Errorf("ledger: remaining reduce: %w", err)
		}
	}

	if remaining.Sign() > 0 {
		// Flipped: the fill exceeded the position — open a new one in the fill's
		// direction for the remainder (that remainder realizes nothing).
		p.dir = fillDir
		p.lots = []lot{{qty: remaining, px: px}}
	} else if len(p.lots) == 0 {
		p.dir = 0 // fully closed -> flat
	}
	return realized, nil
}

// Position returns the symbol's signed open quantity (+ long / - short / 0 flat),
// summed across its open lots. Used by reconcile checks and tests.
func (a *Accountant) Position(symbol string) (Decimal, error) {
	p := a.pos[symbol]
	if p == nil || p.dir == 0 {
		return orders.ZeroDecimal, nil
	}
	total := orders.ZeroDecimal
	for _, l := range p.lots {
		var err error
		if total, err = total.Add(l.qty); err != nil {
			return orders.ZeroDecimal, fmt.Errorf("ledger: sum position: %w", err)
		}
	}
	if p.dir < 0 {
		total = total.Neg()
	}
	return total, nil
}
