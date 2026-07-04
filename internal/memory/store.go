// Package memory is BUCKS's lean, portable memory: a single-file SQLite store of
// what worked and what burned the trader, mirrored into an Obsidian-compatible
// vault where the wikilink graph IS the knowledge graph. It is deliberately NOT a
// copy of Hydra's full nomic+vec0 stack — just the basics that ship (SQLite +
// Obsidian KG), so memory survives a zip -> ship -> unpack round trip.
//
// Two truths the spec demands of this package:
//   - Money is never float64. Every monetary field is an orders.Decimal stored as
//     decimal TEXT, so a drift-prone value round-trips exact through SQLite.
//   - A deleted memory is GONE. Forget actually removes the row so it can never be
//     served by any later recall (no soft-delete-still-served bug), and every write
//     is synchronously committed, not stalled.
package memory

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"bucks/internal/brokers"
	"bucks/internal/orders"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (BSD-3); CGO_ENABLED=0 safe
)

// driverName is the database/sql driver registered by modernc.org/sqlite. It is
// pure Go — no cgo — so CGO_ENABLED=0 builds and cross-compiles (e.g. GOOS=windows)
// keep working and the trader still ships as a single static binary.
const driverName = "sqlite"

// TradeMemory is one recorded trade outcome: what worked, or what burned him. The
// money fields (Entry, Exit, PnL) are orders.Decimal and are persisted as decimal
// TEXT — never float64 — so an exact value survives the store unchanged.
type TradeMemory struct {
	ID     int64          // store-assigned row id (0 until persisted)
	Symbol string         // e.g. "AAPL", "BTC-USD"
	Setup  string         // setup/strategy name, e.g. "breakout"
	Entry  orders.Decimal // entry price (decimal TEXT at rest)
	Exit   orders.Decimal // exit price (decimal TEXT at rest)
	PnL    orders.Decimal // realized PnL (decimal TEXT at rest)
	Lesson string         // the human lesson ("what worked / what burned him")
	TS     time.Time      // when it happened (UTC, stored as RFC3339Nano)
}

// MarketMemory is one market observation tied to a symbol.
type MarketMemory struct {
	ID          int64     // store-assigned row id (0 until persisted)
	Symbol      string    // instrument the observation is about
	Observation string    // free-text note
	TS          time.Time // when observed (UTC, RFC3339Nano)
}

// TradeFilter narrows a RecallTrades query. A zero-value filter matches all rows.
// Fields are AND-combined; empty string fields are ignored.
type TradeFilter struct {
	Symbol string // exact-match symbol, or "" for any
	Setup  string // exact-match setup, or "" for any
	Limit  int    // max rows (0 = no limit); newest first
}

// MarketFilter narrows a RecallMarket query (zero value matches all).
type MarketFilter struct {
	Symbol string // exact-match symbol, or "" for any
	Limit  int    // max rows (0 = no limit); newest first
}

// Store is the single-file SQLite memory store. It is safe for concurrent use:
// the underlying *sql.DB pools connections and serializes writes through SQLite.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (creating if needed) the SQLite memory file at path and ensures the
// schema exists. The connection is configured for synchronous, committed writes
// (no stalled persistence) so a recorded memory is durable immediately.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("memory: empty store path")
	}
	// busy_timeout avoids spurious SQLITE_BUSY under concurrent writers;
	// synchronous(2)=FULL fsyncs every commit (durable against an OS/power crash,
	// not just an app crash) and journal_mode(WAL) keeps writes durable. Memory
	// writes are per-trade, not a hot path, so the extra fsync cost is fine — and
	// it makes the package's "every write is synchronously committed" claim TRUE.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(2)"
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("memory: open %q: %w", path, err)
	}
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle. Safe to call on a nil-db Store.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Path returns the SQLite file path backing this store.
func (s *Store) Path() string { return s.path }

func (s *Store) migrate() error {
	const ddl = `
CREATE TABLE IF NOT EXISTS trade_memory (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	symbol  TEXT NOT NULL,
	setup   TEXT NOT NULL,
	entry   TEXT NOT NULL,
	exit    TEXT NOT NULL,
	pnl     TEXT NOT NULL,
	lesson  TEXT NOT NULL,
	ts      TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS market_memory (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	symbol      TEXT NOT NULL,
	observation TEXT NOT NULL,
	ts          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_trade_symbol ON trade_memory(symbol);
CREATE INDEX IF NOT EXISTS idx_trade_setup  ON trade_memory(setup);
CREATE INDEX IF NOT EXISTS idx_market_symbol ON market_memory(symbol);
CREATE TABLE IF NOT EXISTS broker_fills (
	id      TEXT PRIMARY KEY,
	symbol  TEXT NOT NULL,
	side    TEXT NOT NULL,
	qty     TEXT NOT NULL,
	px      TEXT NOT NULL,
	ts      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_broker_fills_ts ON broker_fills(ts);
CREATE TABLE IF NOT EXISTS broker_reconcile_state (
	id         INTEGER PRIMARY KEY CHECK (id = 1),
	cursor     TEXT NOT NULL,
	basis      TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
`
	if _, err := s.db.Exec(ddl); err != nil {
		return fmt.Errorf("memory: migrate: %w", err)
	}
	return nil
}

// RememberTrade records a trade outcome and returns it with its assigned ID. The
// money fields are written as decimal TEXT (Decimal.String()), never float64. The
// write is committed before this returns (synchronous persistence).
func (s *Store) RememberTrade(t TradeMemory) (TradeMemory, error) {
	if t.Symbol == "" {
		return TradeMemory{}, errors.New("memory: trade symbol required")
	}
	if t.TS.IsZero() {
		t.TS = time.Now().UTC()
	}
	res, err := s.db.Exec(
		`INSERT INTO trade_memory (symbol, setup, entry, exit, pnl, lesson, ts)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.Symbol, t.Setup,
		t.Entry.String(), t.Exit.String(), t.PnL.String(),
		t.Lesson, t.TS.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return TradeMemory{}, fmt.Errorf("memory: remember trade: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return TradeMemory{}, fmt.Errorf("memory: trade id: %w", err)
	}
	t.ID = id
	return t, nil
}

// BrokerFillSeen reports whether the authoritative broker activity id has already
// been applied to the realized-P&L ledger. It lets the reconciler skip duplicate
// fills before mutating its in-memory FIFO basis.
func (s *Store) BrokerFillSeen(id string) (bool, error) {
	if id == "" {
		return false, errors.New("memory: broker fill id required")
	}
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM broker_fills WHERE id = ?`, id).Scan(&one)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("memory: broker fill seen %q: %w", id, err)
}

// BrokerReconcileState returns the durable broker-fill replay cursor and FIFO
// basis. ok=false means the store has not been bootstrapped yet.
func (s *Store) BrokerReconcileState() (cursor time.Time, basis []byte, ok bool, err error) {
	var cursorS, basisS string
	err = s.db.QueryRow(`SELECT cursor, basis FROM broker_reconcile_state WHERE id = 1`).Scan(&cursorS, &basisS)
	if err == nil {
		cursor, perr := time.Parse(time.RFC3339Nano, cursorS)
		if perr != nil {
			return time.Time{}, nil, false, fmt.Errorf("memory: broker reconcile cursor parse %q: %w", cursorS, perr)
		}
		return cursor.UTC(), []byte(basisS), true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil, false, nil
	}
	return time.Time{}, nil, false, fmt.Errorf("memory: broker reconcile state: %w", err)
}

// SaveBrokerReconcileState durably records the broker-fill replay cursor and the
// FIFO basis snapshot that is authoritative at that cursor.
func (s *Store) SaveBrokerReconcileState(cursor time.Time, basis []byte) error {
	if cursor.IsZero() {
		return errors.New("memory: broker reconcile cursor required")
	}
	if basis == nil {
		return errors.New("memory: broker reconcile basis required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("memory: broker reconcile state begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := saveBrokerReconcileStateTx(tx, cursor, basis); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memory: broker reconcile state commit: %w", err)
	}
	committed = true
	return nil
}

// RememberSeededBrokerFills atomically marks first-boot seed fills as seen
// and persists the seeded FIFO basis at cursor. It writes no realized trade rows:
// these fills are already reflected in broker.Positions() and the seeded basis.
func (s *Store) RememberSeededBrokerFills(fills []brokers.Fill, cursor time.Time, basis []byte) error {
	if cursor.IsZero() {
		return errors.New("memory: broker reconcile cursor required")
	}
	if basis == nil {
		return errors.New("memory: broker reconcile basis required")
	}
	cursorUTC := cursor.UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("memory: seeded broker fills begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	for _, f := range fills {
		if f.ID == "" {
			return errors.New("memory: broker fill id required")
		}
		if f.Symbol == "" {
			return errors.New("memory: broker fill symbol required")
		}
		if f.At.IsZero() {
			return fmt.Errorf("memory: broker fill %s timestamp required", f.ID)
		}
		if f.At.After(cursorUTC) {
			return fmt.Errorf("memory: seeded broker fill %s at %s is after cursor %s", f.ID, f.At.UTC().Format(time.RFC3339Nano), cursorUTC.Format(time.RFC3339Nano))
		}
		if f.Qty.Sign() <= 0 {
			return fmt.Errorf("memory: broker fill %s qty must be positive", f.ID)
		}
		if f.Px.Sign() <= 0 {
			return fmt.Errorf("memory: broker fill %s price must be positive", f.ID)
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO broker_fills (id, symbol, side, qty, px, ts)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			f.ID, f.Symbol, f.Side.String(), f.Qty.String(), f.Px.String(), f.At.UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("memory: seeded broker fill insert: %w", err)
		}
	}
	if err := saveBrokerReconcileStateTx(tx, cursorUTC, basis); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memory: seeded broker fills commit: %w", err)
	}
	committed = true
	return nil
}

// RememberBrokerFill atomically marks a broker fill id as seen and, when realized
// is non-zero, persists the realized P&L row that feeds the daily-loss breaker,
// plus the replay cursor/FIFO basis that are authoritative after the fill.
// Duplicate ids return inserted=false and do not add a trade row or state update.
func (s *Store) RememberBrokerFill(id, symbol string, side orders.Side, qty, px, realized orders.Decimal, at, cursor time.Time, basis []byte) (inserted bool, err error) {
	if id == "" {
		return false, errors.New("memory: broker fill id required")
	}
	if symbol == "" {
		return false, errors.New("memory: broker fill symbol required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if cursor.IsZero() {
		cursor = at.UTC()
	}
	if basis == nil {
		return false, errors.New("memory: broker reconcile basis required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("memory: broker fill begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	res, err := tx.Exec(
		`INSERT OR IGNORE INTO broker_fills (id, symbol, side, qty, px, ts)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, symbol, side.String(), qty.String(), px.String(), at.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return false, fmt.Errorf("memory: broker fill insert: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("memory: broker fill rows affected: %w", err)
	}
	if rows == 0 {
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("memory: broker fill duplicate commit: %w", err)
		}
		committed = true
		return false, nil
	}

	if realized.Sign() != 0 {
		if _, err := tx.Exec(
			`INSERT INTO trade_memory (symbol, setup, entry, exit, pnl, lesson, ts)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			symbol, "live",
			orders.ZeroDecimal.String(), px.String(), realized.String(),
			"realized on broker fill", at.UTC().Format(time.RFC3339Nano),
		); err != nil {
			return false, fmt.Errorf("memory: broker fill trade insert: %w", err)
		}
	}
	if err := saveBrokerReconcileStateTx(tx, cursor, basis); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("memory: broker fill commit: %w", err)
	}
	committed = true
	return true, nil
}

func saveBrokerReconcileStateTx(tx *sql.Tx, cursor time.Time, basis []byte) error {
	_, err := tx.Exec(
		`INSERT OR REPLACE INTO broker_reconcile_state (id, cursor, basis, updated_at)
		 VALUES (1, ?, ?, ?)`,
		cursor.UTC().Format(time.RFC3339Nano),
		string(basis),
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("memory: broker reconcile state save: %w", err)
	}
	return nil
}

// RealizedPnLSince sums the realized P&L of every trade recorded at or after
// `since` (inclusive). It is the LIVE source for the risk engine's daily-loss
// circuit breaker, which was previously fed a hardcoded zero and so could never
// fire. Persisting realized P&L here (not just in-memory) means the day's loss
// budget survives a process restart.
//
// ts is stored as variable-width RFC3339Nano, so a raw string comparison is NOT
// order-safe; the query bounds the scan by the fixed-width YYYY-MM-DD prefix, then
// filters to the exact instant and sums in exact decimal (never float64).
func (s *Store) RealizedPnLSince(since time.Time) (orders.Decimal, error) {
	sinceUTC := since.UTC()
	rows, err := s.db.Query(
		`SELECT pnl, ts FROM trade_memory WHERE substr(ts, 1, 10) >= ?`,
		sinceUTC.Format("2006-01-02"),
	)
	if err != nil {
		return orders.ZeroDecimal, fmt.Errorf("memory: realized pnl query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	total := orders.ZeroDecimal
	for rows.Next() {
		var pnlS, tsS string
		if err := rows.Scan(&pnlS, &tsS); err != nil {
			return orders.ZeroDecimal, fmt.Errorf("memory: realized pnl scan: %w", err)
		}
		ts, err := time.Parse(time.RFC3339Nano, tsS)
		if err != nil {
			return orders.ZeroDecimal, fmt.Errorf("memory: realized pnl parse ts %q: %w", tsS, err)
		}
		if ts.Before(sinceUTC) {
			continue // inside the boundary date but before the exact cutoff
		}
		pnl, err := orders.ParseDecimal(pnlS)
		if err != nil {
			return orders.ZeroDecimal, fmt.Errorf("memory: realized pnl parse %q: %w", pnlS, err)
		}
		if total, err = total.Add(pnl); err != nil {
			return orders.ZeroDecimal, fmt.Errorf("memory: realized pnl sum: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return orders.ZeroDecimal, fmt.Errorf("memory: realized pnl rows: %w", err)
	}
	return total, nil
}

// RememberMarket records a market observation and returns it with its assigned ID.
func (s *Store) RememberMarket(m MarketMemory) (MarketMemory, error) {
	if m.Symbol == "" {
		return MarketMemory{}, errors.New("memory: market symbol required")
	}
	if m.TS.IsZero() {
		m.TS = time.Now().UTC()
	}
	res, err := s.db.Exec(
		`INSERT INTO market_memory (symbol, observation, ts) VALUES (?, ?, ?)`,
		m.Symbol, m.Observation, m.TS.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return MarketMemory{}, fmt.Errorf("memory: remember market: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return MarketMemory{}, fmt.Errorf("memory: market id: %w", err)
	}
	m.ID = id
	return m, nil
}

// RecallTrades returns trades matching the filter, newest first. Money fields are
// parsed back from decimal TEXT to exact orders.Decimal. A forgotten row never
// appears here — the recall reads live table state, with no soft-delete shadow.
func (s *Store) RecallTrades(f TradeFilter) ([]TradeMemory, error) {
	q := `SELECT id, symbol, setup, entry, exit, pnl, lesson, ts FROM trade_memory`
	var args []any
	var where []string
	if f.Symbol != "" {
		where = append(where, "symbol = ?")
		args = append(args, f.Symbol)
	}
	if f.Setup != "" {
		where = append(where, "setup = ?")
		args = append(args, f.Setup)
	}
	for i, w := range where {
		if i == 0 {
			q += " WHERE " + w
		} else {
			q += " AND " + w
		}
	}
	q += " ORDER BY id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("memory: recall trades: %w", err)
	}
	defer rows.Close()

	var out []TradeMemory
	for rows.Next() {
		var (
			t                   TradeMemory
			entryS, exitS, pnlS string
			tsS                 string
		)
		if err := rows.Scan(&t.ID, &t.Symbol, &t.Setup, &entryS, &exitS, &pnlS, &t.Lesson, &tsS); err != nil {
			return nil, fmt.Errorf("memory: scan trade: %w", err)
		}
		if t.Entry, err = orders.ParseDecimal(entryS); err != nil {
			return nil, fmt.Errorf("memory: parse entry %q: %w", entryS, err)
		}
		if t.Exit, err = orders.ParseDecimal(exitS); err != nil {
			return nil, fmt.Errorf("memory: parse exit %q: %w", exitS, err)
		}
		if t.PnL, err = orders.ParseDecimal(pnlS); err != nil {
			return nil, fmt.Errorf("memory: parse pnl %q: %w", pnlS, err)
		}
		if t.TS, err = time.Parse(time.RFC3339Nano, tsS); err != nil {
			return nil, fmt.Errorf("memory: parse ts %q: %w", tsS, err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: recall trades rows: %w", err)
	}
	return out, nil
}

// RecallMarket returns market observations matching the filter, newest first.
func (s *Store) RecallMarket(f MarketFilter) ([]MarketMemory, error) {
	q := `SELECT id, symbol, observation, ts FROM market_memory`
	var args []any
	if f.Symbol != "" {
		q += " WHERE symbol = ?"
		args = append(args, f.Symbol)
	}
	q += " ORDER BY id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("memory: recall market: %w", err)
	}
	defer rows.Close()

	var out []MarketMemory
	for rows.Next() {
		var (
			m   MarketMemory
			tsS string
		)
		if err := rows.Scan(&m.ID, &m.Symbol, &m.Observation, &tsS); err != nil {
			return nil, fmt.Errorf("memory: scan market: %w", err)
		}
		if m.TS, err = time.Parse(time.RFC3339Nano, tsS); err != nil {
			return nil, fmt.Errorf("memory: parse ts %q: %w", tsS, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: recall market rows: %w", err)
	}
	return out, nil
}

// Forget permanently removes a trade memory by id. This is a hard DELETE: after it
// returns, the row is gone from the table and can never be served by any later
// recall (the spec's "a deleted memory is gone from recall" guarantee — no
// FTS/soft-delete path keeps serving it). Returns ErrNotFound if no such row.
func (s *Store) Forget(id int64) error {
	res, err := s.db.Exec(`DELETE FROM trade_memory WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("memory: forget %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("memory: forget rows %d: %w", id, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ForgetMarket permanently removes a market memory by id. Like Forget, this is a
// hard DELETE: after it returns, the row is gone from market_memory and can never
// be served by any later RecallMarket (the same "a deleted memory is gone from
// recall" guarantee Forget gives trades — market_memory is no longer the one table
// with no delete path). Returns ErrNotFound if no such row.
func (s *Store) ForgetMarket(id int64) error {
	res, err := s.db.Exec(`DELETE FROM market_memory WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("memory: forget market %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("memory: forget market rows %d: %w", id, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ErrNotFound is returned by Forget/ForgetMarket when the target id does not exist.
var ErrNotFound = errors.New("memory: not found")
