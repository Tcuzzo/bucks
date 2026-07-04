package ledger

import (
	"context"
	"fmt"
	"sort"
	"time"

	"bucks/internal/brokers"
	"bucks/internal/orders"
)

// BrokerFillStore is the durable side of broker-fill reconciliation. The store
// must atomically persist the broker activity id, any realized P&L row, and the
// next cursor/FIFO basis in one transaction.
type BrokerFillStore interface {
	BrokerFillSeen(id string) (bool, error)
	RememberBrokerFill(id, symbol string, side orders.Side, qty, px, realized Decimal, at, cursor time.Time, basis []byte) (bool, error)
	RememberSeededBrokerFills(fills []brokers.Fill, cursor time.Time, basis []byte) error
	BrokerReconcileState() (cursor time.Time, basis []byte, ok bool, err error)
	SaveBrokerReconcileState(cursor time.Time, basis []byte) error
}

// FillReconciler reads authoritative broker fills and applies them to the FIFO
// accountant exactly once.
//
// Cursor/seed/basis model: after the first successful seed, the store's
// cursor+basis row is the single authoritative FIFO source. A boot with durable
// state reloads that basis and replays broker fills strictly after the stored
// cursor, so fills that happened while BUCKS was offline are accounted before
// risk checks. Only a store with no state may be seeded from broker.Positions;
// that bootstrap is immediately persisted with a cursor at the seed snapshot, and
// later boots must not layer broker positions on top of the reloaded basis.
// Durable activity-id de-duplication makes retries/overlap/restarts no-op instead
// of double-counting.
type FillReconciler struct {
	reader          brokers.FillReader
	store           BrokerFillStore
	acct            *Accountant
	after           time.Time
	stateLoaded     bool
	loadedFromStore bool
}

// NewFillReconciler builds a reconciler over the broker fill stream and durable
// store. The caller owns first-run seeding when LoadState reports no durable
// basis.
func NewFillReconciler(reader brokers.FillReader, store BrokerFillStore, after time.Time) *FillReconciler {
	return &FillReconciler{
		reader: reader,
		store:  store,
		acct:   New(),
		after:  after.UTC(),
	}
}

// Seed reconciles an open broker position into the FIFO basis.
func (r *FillReconciler) Seed(symbol string, signedQty, avgPx Decimal) {
	r.acct.Seed(symbol, signedQty, avgPx)
}

// LoadState restores the durable FIFO basis and cursor when they exist. ok=false
// means this store has not been bootstrapped yet, so the caller may seed current
// broker positions and then SaveState. Store read/decode errors are returned so
// the live loop can fail closed instead of trading on an empty basis.
func (r *FillReconciler) LoadState() (bool, error) {
	if r == nil {
		return false, nil
	}
	if r.stateLoaded {
		return r.loadedFromStore, nil
	}
	if r.store == nil {
		return false, fmt.Errorf("ledger: fill reconciler store is nil")
	}
	cursor, basis, ok, err := r.store.BrokerReconcileState()
	if err != nil {
		return false, fmt.Errorf("ledger: load broker reconcile state: %w", err)
	}
	if ok {
		if err := r.acct.restoreBasis(basis); err != nil {
			return false, err
		}
		r.after = cursor.UTC()
		r.loadedFromStore = true
	}
	r.stateLoaded = true
	return ok, nil
}

// SaveState durably records the current FIFO basis at cursor. It is used by the
// first-run broker-position seed before the loop is allowed to trade.
func (r *FillReconciler) SaveState(cursor time.Time) error {
	if r == nil {
		return nil
	}
	if r.store == nil {
		return fmt.Errorf("ledger: fill reconciler store is nil")
	}
	basis, err := r.acct.encodeBasis()
	if err != nil {
		return err
	}
	cursor = cursor.UTC()
	if err := r.store.SaveBrokerReconcileState(cursor, basis); err != nil {
		return err
	}
	r.after = cursor
	r.stateLoaded = true
	r.loadedFromStore = true
	return nil
}

// SaveSeedState atomically records the seeded FIFO basis and marks venue fills
// already reflected by that seed as seen. It intentionally writes no realized
// rows for those fills; the broker position seed already contains their basis.
func (r *FillReconciler) SaveSeedState(cursor time.Time, fills []brokers.Fill) error {
	if r == nil {
		return nil
	}
	if r.store == nil {
		return fmt.Errorf("ledger: fill reconciler store is nil")
	}
	basis, err := r.acct.encodeBasis()
	if err != nil {
		return err
	}
	cursor = cursor.UTC()
	seeded := make([]brokers.Fill, 0, len(fills))
	for _, f := range fills {
		if f.At.After(cursor) {
			continue
		}
		seeded = append(seeded, f)
	}
	sort.SliceStable(seeded, func(i, j int) bool {
		if seeded[i].At.Equal(seeded[j].At) {
			return seeded[i].ID < seeded[j].ID
		}
		return seeded[i].At.Before(seeded[j].At)
	})
	if err := r.store.RememberSeededBrokerFills(seeded, cursor, basis); err != nil {
		return err
	}
	r.after = cursor
	r.stateLoaded = true
	r.loadedFromStore = true
	return nil
}

// Position returns the current in-memory reconciler position for tests and
// consistency checks.
func (r *FillReconciler) Position(symbol string) (Decimal, error) {
	return r.acct.Position(symbol)
}

// Reconcile reads new broker fills and applies each unseen activity id exactly
// once. Any read, store, or accounting error is returned loudly so the caller can
// skip the tick instead of feeding a false zero to the daily-loss breaker.
func (r *FillReconciler) Reconcile(ctx context.Context) error {
	if r == nil || r.reader == nil {
		return nil
	}
	if r.store == nil {
		return fmt.Errorf("ledger: fill reconciler store is nil")
	}
	if _, err := r.LoadState(); err != nil {
		return err
	}
	fills, err := r.reader.FillsSince(ctx, r.queryAfter())
	if err != nil {
		return fmt.Errorf("ledger: read broker fills: %w", err)
	}
	sort.SliceStable(fills, func(i, j int) bool {
		if fills[i].At.Equal(fills[j].At) {
			return fills[i].ID < fills[j].ID
		}
		return fills[i].At.Before(fills[j].At)
	})
	for _, f := range fills {
		if f.ID == "" {
			return fmt.Errorf("ledger: broker fill missing activity id for %s", f.Symbol)
		}
		seen, err := r.store.BrokerFillSeen(f.ID)
		if err != nil {
			return err
		}
		if seen {
			if err := r.saveCurrentState(f.At); err != nil {
				return err
			}
			continue
		}
		prev := r.acct.snapshot(f.Symbol)
		realized, err := r.acct.Apply(f.Symbol, f.Side, f.Qty, f.Px)
		if err != nil {
			return fmt.Errorf("ledger: apply broker fill %s: %w", f.ID, err)
		}
		nextCursor := r.nextCursor(f.At)
		basis, err := r.acct.encodeBasis()
		if err != nil {
			r.acct.restore(f.Symbol, prev)
			return err
		}
		inserted, err := r.store.RememberBrokerFill(f.ID, f.Symbol, f.Side, f.Qty, f.Px, realized, f.At, nextCursor, basis)
		if err != nil {
			r.acct.restore(f.Symbol, prev)
			return err
		}
		if !inserted {
			r.acct.restore(f.Symbol, prev)
			continue
		}
		r.after = nextCursor
	}
	return nil
}

func (r *FillReconciler) saveCurrentState(at time.Time) error {
	nextCursor := r.nextCursor(at)
	if nextCursor.Equal(r.after) {
		return nil
	}
	basis, err := r.acct.encodeBasis()
	if err != nil {
		return err
	}
	if err := r.store.SaveBrokerReconcileState(nextCursor, basis); err != nil {
		return err
	}
	r.after = nextCursor
	return nil
}

func (r *FillReconciler) nextCursor(at time.Time) time.Time {
	if at.After(r.after) {
		return at.UTC()
	}
	return r.after
}

func (r *FillReconciler) queryAfter() time.Time {
	if r.after.IsZero() {
		return r.after
	}
	return r.after.Add(-time.Nanosecond)
}
