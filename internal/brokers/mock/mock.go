// Package mock is an in-memory, deterministic implementation of brokers.Broker.
// It exists for tests and for driving the rest of the BUCKS build without a live
// venue. Tests seed account/positions/quotes and assert what was placed/canceled.
//
// Idempotency: PlaceOrder is keyed on ClOrdID. Placing the SAME ClOrdID twice
// returns the SAME BrokerOrder and creates no duplicate — mirroring the real
// broker contract so reconcile/retry logic can be exercised end-to-end offline.
package mock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"bucks/internal/brokers"
	"bucks/internal/orders"
)

// MockBroker is a deterministic, concurrency-safe in-memory broker.
type MockBroker struct {
	mu sync.Mutex

	account   brokers.Account
	positions map[string]brokers.Position // by symbol
	quotes    map[string]brokers.Quote    // by symbol

	// orders is keyed by ClOrdID — the idempotency map.
	orders map[string]brokers.BrokerOrder
	// placeOrder records the order in which ClOrdIDs were first placed, so tests
	// can assert sequence deterministically.
	placedOrder []string
	// canceled records ClOrdIDs that have been canceled, in cancel order.
	canceled []string
	// fills is the authoritative broker activity stream used by the reconciler.
	fills []brokers.Fill

	// seq drives deterministic broker order ids (no time, no rand).
	seq int

	// optional hooks let a test force errors without real network. Nil = no-op.
	getOrderErr map[string]error // ClOrdID -> error returned by GetOrder
}

// New constructs an empty MockBroker with zeroed account and no positions.
func New() *MockBroker {
	return &MockBroker{
		positions:   make(map[string]brokers.Position),
		quotes:      make(map[string]brokers.Quote),
		orders:      make(map[string]brokers.BrokerOrder),
		getOrderErr: make(map[string]error),
	}
}

// SetAccount seeds the account snapshot returned by Account.
func (m *MockBroker) SetAccount(a brokers.Account) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.account = a
}

// SetPosition seeds (or overwrites) a position for symbol. A zero qty removes it.
func (m *MockBroker) SetPosition(p brokers.Position) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.Qty.IsZero() {
		delete(m.positions, p.Symbol)
		return
	}
	m.positions[p.Symbol] = p
}

// SetQuote seeds the quote returned by Quote for its symbol.
func (m *MockBroker) SetQuote(q brokers.Quote) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quotes[q.Symbol] = q
}

// SetFills seeds the authoritative broker fill stream returned by FillsSince.
// Output is sorted by fill timestamp for deterministic tests.
func (m *MockBroker) SetFills(fills []brokers.Fill) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fills = append(m.fills[:0], fills...)
	sortFills(m.fills)
}

// SeedBrokerOrder injects a broker-side order keyed by ClOrdID. This lets a test
// model "the broker already has this order in state X" (e.g. an in-flight intent
// that actually filled while BUCKS was down) without going through PlaceOrder.
func (m *MockBroker) SeedBrokerOrder(o brokers.BrokerOrder) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.orders[o.ClOrdID]; !exists {
		m.placedOrder = append(m.placedOrder, o.ClOrdID)
	}
	m.orders[o.ClOrdID] = o
}

// FailGetOrder forces GetOrder(clOrdID) to return err (for error-path tests).
func (m *MockBroker) FailGetOrder(clOrdID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getOrderErr[clOrdID] = err
}

// Placed returns the ClOrdIDs placed so far, in first-placed order (a copy).
func (m *MockBroker) Placed() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.placedOrder))
	copy(out, m.placedOrder)
	return out
}

// Canceled returns the ClOrdIDs canceled so far, in cancel order (a copy).
func (m *MockBroker) Canceled() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.canceled))
	copy(out, m.canceled)
	return out
}

// Account implements brokers.Broker.
func (m *MockBroker) Account(ctx context.Context) (brokers.Account, error) {
	if err := ctx.Err(); err != nil {
		return brokers.Account{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.account, nil
}

// Positions implements brokers.Broker. Output is symbol-sorted for determinism.
func (m *MockBroker) Positions(ctx context.Context) ([]brokers.Position, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]brokers.Position, 0, len(m.positions))
	for _, p := range m.positions {
		out = append(out, p)
	}
	sortPositions(out)
	return out, nil
}

// Quote implements brokers.Broker.
func (m *MockBroker) Quote(ctx context.Context, symbol string) (brokers.Quote, error) {
	if err := ctx.Err(); err != nil {
		return brokers.Quote{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	q, ok := m.quotes[symbol]
	if !ok {
		return brokers.Quote{}, fmt.Errorf("mock: no quote seeded for %q", symbol)
	}
	return q, nil
}

// PlaceOrder implements brokers.Broker. It is IDEMPOTENT on req.ClOrdID: a second
// place with the same ClOrdID returns the existing BrokerOrder unchanged and adds
// no duplicate. A fresh order is recorded in StatusNew with the request's qty.
func (m *MockBroker) PlaceOrder(ctx context.Context, req brokers.OrderRequest) (brokers.BrokerOrder, error) {
	if err := ctx.Err(); err != nil {
		return brokers.BrokerOrder{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// Idempotency: same ClOrdID => same order, no duplicate.
	if existing, ok := m.orders[req.ClOrdID]; ok {
		return existing, nil
	}

	m.seq++
	bo := brokers.BrokerOrder{
		BrokerOrderID: fmt.Sprintf("mock-%d", m.seq),
		ClOrdID:       req.ClOrdID,
		Symbol:        req.Symbol,
		Side:          req.Side,
		Status:        brokers.StatusNew,
		OrderQty:      req.Qty,
		CumQty:        orders.ZeroDecimal,
		AvgPx:         orders.ZeroDecimal,
	}
	m.orders[req.ClOrdID] = bo
	m.placedOrder = append(m.placedOrder, req.ClOrdID)
	return bo, nil
}

// CancelOrder implements brokers.Broker. It marks a known order Canceled; an
// unknown ClOrdID returns brokers.ErrOrderNotFound.
func (m *MockBroker) CancelOrder(ctx context.Context, clOrdID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	bo, ok := m.orders[clOrdID]
	if !ok {
		return brokers.ErrOrderNotFound
	}
	bo.Status = brokers.StatusCanceled
	m.orders[clOrdID] = bo
	m.canceled = append(m.canceled, clOrdID)
	return nil
}

// GetOrder implements brokers.Broker. Returns brokers.ErrOrderNotFound for an
// unknown ClOrdID, or a forced error if FailGetOrder was set.
func (m *MockBroker) GetOrder(ctx context.Context, clOrdID string) (brokers.BrokerOrder, error) {
	if err := ctx.Err(); err != nil {
		return brokers.BrokerOrder{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err, ok := m.getOrderErr[clOrdID]; ok && err != nil {
		return brokers.BrokerOrder{}, err
	}
	bo, ok := m.orders[clOrdID]
	if !ok {
		return brokers.BrokerOrder{}, brokers.ErrOrderNotFound
	}
	return bo, nil
}

// FillsSince implements brokers.FillReader.
func (m *MockBroker) FillsSince(ctx context.Context, after time.Time) ([]brokers.Fill, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]brokers.Fill, 0, len(m.fills))
	for _, f := range m.fills {
		if f.At.After(after) {
			out = append(out, f)
		}
	}
	sortFills(out)
	return out, nil
}

// compile-time assertion that MockBroker satisfies the interface.
var _ brokers.Broker = (*MockBroker)(nil)
var _ brokers.FillReader = (*MockBroker)(nil)

// sortPositions orders positions by symbol for deterministic output.
func sortPositions(ps []brokers.Position) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && ps[j-1].Symbol > ps[j].Symbol; j-- {
			ps[j-1], ps[j] = ps[j], ps[j-1]
		}
	}
}

func sortFills(fs []brokers.Fill) {
	for i := 1; i < len(fs); i++ {
		for j := i; j > 0; j-- {
			if fs[j-1].At.Before(fs[j].At) || fs[j-1].At.Equal(fs[j].At) && fs[j-1].ID <= fs[j].ID {
				break
			}
			fs[j-1], fs[j] = fs[j], fs[j-1]
		}
	}
}
