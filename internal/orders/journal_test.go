package orders

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func tmpJournal(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "orders.wal")
}

// recordingSyncer records the order of sync calls relative to a "send" event so
// the test can assert AppendIntent fsync'd BEFORE the network send ran.
type recordingSyncer struct {
	mu     sync.Mutex
	events *[]string
	syncs  int64
}

func (r *recordingSyncer) Sync() error {
	atomic.AddInt64(&r.syncs, 1)
	r.mu.Lock()
	*r.events = append(*r.events, "sync")
	r.mu.Unlock()
	return nil
}

// AppendIntent must fsync BEFORE it returns (so a caller can safely send only
// after AppendIntent returns). We assert the sync happened during AppendIntent
// and that a subsequent "send" is ordered strictly after the sync.
func TestAppendIntent_SyncsBeforeSend(t *testing.T) {
	path := tmpJournal(t)
	var events []string
	rec := &recordingSyncer{events: &events}
	j, err := openWithSyncer(path, rec)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer j.Close()

	if err := j.AppendIntent(IntentRecord{
		ClOrdID:  ClientOrderID("momentum", "AAPL", "entry", 1),
		Strategy: "momentum", Symbol: "AAPL", Side: SideBuy,
		Qty: dec(t, "100"), Px: dec(t, "150.00"),
	}); err != nil {
		t.Fatalf("AppendIntent: %v", err)
	}

	// By the time AppendIntent returned, the sync MUST already have happened.
	if got := atomic.LoadInt64(&rec.syncs); got != 1 {
		t.Fatalf("syncs after AppendIntent = %d, want 1 (must fsync before returning)", got)
	}

	// Now the caller does its network send.
	events = append(events, "send")

	if len(events) != 2 || events[0] != "sync" || events[1] != "send" {
		t.Fatalf("ordering wrong: %v, want [sync send]", events)
	}
}

// A full lifecycle (intent -> sent -> ack -> partial fill -> full fill) replays
// to the exact terminal order state, not in-flight.
func TestReplay_FullLifecycle(t *testing.T) {
	path := tmpJournal(t)
	j, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	id := ClientOrderID("momentum", "AAPL", "entry", 7)
	mustNil(t, j.AppendIntent(IntentRecord{ClOrdID: id, Strategy: "momentum", Symbol: "AAPL", Side: SideBuy, Qty: dec(t, "100"), Px: dec(t, "150")}))
	mustNil(t, j.AppendSent(id))
	mustNil(t, j.AppendAck(id))
	mustNil(t, j.AppendFill(id, "f1", dec(t, "40"), dec(t, "150")))
	mustNil(t, j.AppendFill(id, "f2", dec(t, "60"), dec(t, "151")))
	mustNil(t, j.Close())

	res, err := Replay(path)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d orders, want 1", len(res))
	}
	o := res[0]
	if o.InFlight {
		t.Fatalf("fully-filled order should not be in-flight")
	}
	if o.State != StateFilled {
		t.Fatalf("state=%s, want Filled", o.State)
	}
	assertDecEq(t, o.CumQty, "100")
	assertDecEq(t, o.LeavesQty, "0")
	// (40*150 + 60*151)/100 = (6000+9060)/100 = 15060/100 = 150.6
	assertDecEq(t, o.AvgPx, "150.6")
	if o.ClOrdID != id {
		t.Fatalf("clOrdID=%s, want %s", o.ClOrdID, id)
	}
	// The BUY side must survive the WAL round-trip. SideBuy is the int zero value,
	// so this fails if the Record reintroduces json:"side,omitempty".
	if o.Side != SideBuy {
		t.Fatalf("side=%v, want Buy", o.Side)
	}
}

// TestAppendIntent_BuySidePersistedOnDisk is the test that actually BITES the
// json:"side,omitempty" bug. The in-memory replay reconstructs a BUY side
// correctly by zero-value coincidence (omitempty drops SideBuy(0), and the
// missing key unmarshals back to 0=SideBuy), so a round-trip assertion can't
// catch the bug. The real harm is on disk: with omitempty the durable INTENT
// record for a BUY has NO "side" key at all — invisible to any log/audit reader
// and fragile if the enum is ever reordered. This asserts the key is present in
// the serialized record.
func TestAppendIntent_BuySidePersistedOnDisk(t *testing.T) {
	path := tmpJournal(t)
	j, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	id := ClientOrderID("momentum", "AAPL", "entry", 1)
	mustNil(t, j.AppendIntent(IntentRecord{ClOrdID: id, Strategy: "momentum", Symbol: "AAPL", Side: SideBuy, Qty: dec(t, "10"), Px: dec(t, "100")}))
	mustNil(t, j.Close())

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	line := data
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		line = data[:i] // first line is the INTENT record
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("unmarshal intent line %q: %v", line, err)
	}
	if _, ok := m["side"]; !ok {
		t.Fatalf("BUY INTENT record is missing the \"side\" key on disk (omitempty bug): %s", line)
	}
}

// Crash recovery: INTENT written, process "dies" (no further records). Replay
// surfaces the order as in-flight.
func TestReplay_CrashAfterIntent_InFlight(t *testing.T) {
	path := tmpJournal(t)
	j, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	id := ClientOrderID("breakout", "TSLA", "entry", 3)
	mustNil(t, j.AppendIntent(IntentRecord{ClOrdID: id, Strategy: "breakout", Symbol: "TSLA", Side: SideBuy, Qty: dec(t, "10"), Px: dec(t, "700")}))
	// Simulate crash: we do NOT write SENT/ACK/etc. Close to flush what we have.
	mustNil(t, j.Close())

	res, err := Replay(path)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d orders, want 1", len(res))
	}
	if !res[0].InFlight {
		t.Fatalf("order with INTENT and no terminal event must be in-flight")
	}
	if res[0].State != StateNew {
		t.Fatalf("state=%s, want New", res[0].State)
	}
}

// A SENT-but-not-terminal order is also in-flight (ack'd at broker but no fill/cancel).
func TestReplay_SentNoTerminal_InFlight(t *testing.T) {
	path := tmpJournal(t)
	j, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	id := ClientOrderID("mr", "BTC/USD", "exit", 9)
	mustNil(t, j.AppendIntent(IntentRecord{ClOrdID: id, Strategy: "mr", Symbol: "BTC/USD", Side: SideSell, Qty: dec(t, "1"), Px: dec(t, "60000")}))
	mustNil(t, j.AppendSent(id))
	mustNil(t, j.AppendAck(id))
	mustNil(t, j.AppendFill(id, "p1", dec(t, "0.4"), dec(t, "60000"))) // partial only
	mustNil(t, j.Close())

	res, err := Replay(path)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !res[0].InFlight {
		t.Fatalf("partially-filled non-terminal order must be in-flight")
	}
	if res[0].State != StatePartiallyFilled {
		t.Fatalf("state=%s, want PartiallyFilled", res[0].State)
	}
	assertDecEq(t, res[0].CumQty, "0.4")
	if res[0].Side != SideSell {
		t.Fatalf("side=%v, want Sell", res[0].Side)
	}
}

// Terminal via CANCEL / REJECT replays as not-in-flight.
func TestReplay_TerminalCancelReject(t *testing.T) {
	path := tmpJournal(t)
	j, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	idC := ClientOrderID("s", "AAPL", "entry", 1)
	idR := ClientOrderID("s", "AAPL", "entry", 2)
	mustNil(t, j.AppendIntent(IntentRecord{ClOrdID: idC, Strategy: "s", Symbol: "AAPL", Side: SideBuy, Qty: dec(t, "5"), Px: dec(t, "10")}))
	mustNil(t, j.AppendCancel(idC))
	mustNil(t, j.AppendIntent(IntentRecord{ClOrdID: idR, Strategy: "s", Symbol: "AAPL", Side: SideBuy, Qty: dec(t, "5"), Px: dec(t, "10")}))
	mustNil(t, j.AppendReject(idR, "buying power"))
	mustNil(t, j.Close())

	res, err := Replay(path)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d orders, want 2", len(res))
	}
	byID := map[string]*ReplayResult{res[0].ClOrdID: res[0], res[1].ClOrdID: res[1]}
	if byID[idC].State != StateCanceled || byID[idC].InFlight {
		t.Fatalf("cancel order: state=%s inflight=%v", byID[idC].State, byID[idC].InFlight)
	}
	if byID[idR].State != StateRejected || byID[idR].InFlight {
		t.Fatalf("reject order: state=%s inflight=%v", byID[idR].State, byID[idR].InFlight)
	}
}

// A corrupt/truncated TRAILING line (a real crash mid-append) is tolerated:
// the good records before it replay, the partial tail is skipped, no panic, no error.
func TestReplay_TruncatedTrailingLine(t *testing.T) {
	path := tmpJournal(t)
	j, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	id := ClientOrderID("momentum", "AAPL", "entry", 1)
	mustNil(t, j.AppendIntent(IntentRecord{ClOrdID: id, Strategy: "momentum", Symbol: "AAPL", Side: SideBuy, Qty: dec(t, "100"), Px: dec(t, "150")}))
	mustNil(t, j.AppendSent(id))
	mustNil(t, j.Close())

	// Simulate a crash mid-write: append a partial JSON line with NO trailing newline.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := f.WriteString(`{"type":"FILL","clOrdID":"` + id + `","fillID":"f1","qty":"40`); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	mustNil(t, f.Close())

	res, err := Replay(path)
	if err != nil {
		t.Fatalf("replay must tolerate truncated tail, got err: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d orders, want 1", len(res))
	}
	// The partial FILL was skipped: order stays New (intent+sent only), in-flight.
	if res[0].State != StateNew {
		t.Fatalf("state=%s, want New (partial fill must be skipped)", res[0].State)
	}
	if !res[0].InFlight {
		t.Fatalf("order should be in-flight")
	}
	assertDecEq(t, res[0].CumQty, "0")
}

// A corrupt line that is NOT the last line (newline-terminated garbage) is real
// corruption and must error, not silently drop data.
func TestReplay_CorruptMidLine_Errors(t *testing.T) {
	path := tmpJournal(t)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	id := ClientOrderID("s", "AAPL", "entry", 1)
	good := `{"type":"INTENT","clOrdID":"` + id + `","strategy":"s","symbol":"AAPL","qty":"1","px":"1"}` + "\n"
	garbage := "this is not json\n"
	tail := `{"type":"SENT","clOrdID":"` + id + `"}` + "\n"
	if _, err := f.WriteString(good + garbage + tail); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustNil(t, f.Close())

	if _, err := Replay(path); err == nil {
		t.Fatalf("expected error on mid-file corruption, got nil")
	}
}

// Replay of a missing journal returns no orders and no error.
func TestReplay_MissingFile(t *testing.T) {
	res, err := Replay(filepath.Join(t.TempDir(), "nope.wal"))
	if err != nil {
		t.Fatalf("missing file should be nil error, got %v", err)
	}
	if res != nil {
		t.Fatalf("missing file should yield nil results, got %v", res)
	}
}

// Concurrent AppendIntent/AppendFill from many goroutines must not corrupt the
// file: every record replays and all orders reconstruct correctly.
func TestJournal_ConcurrentAppends(t *testing.T) {
	path := tmpJournal(t)
	j, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id := ClientOrderID("momentum", "AAPL", "entry", uint64(i))
			qty := dec(t, "10")
			px := dec(t, "100")
			if err := j.AppendIntent(IntentRecord{ClOrdID: id, Strategy: "momentum", Symbol: "AAPL", Side: SideBuy, Qty: qty, Px: px}); err != nil {
				t.Errorf("goroutine %d intent: %v", i, err)
				return
			}
			// Fully fill it.
			if err := j.AppendFill(id, "fill", qty, px); err != nil {
				t.Errorf("goroutine %d fill: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	mustNil(t, j.Close())

	res, err := Replay(path)
	if err != nil {
		t.Fatalf("replay after concurrent appends: %v", err)
	}
	if len(res) != n {
		t.Fatalf("got %d orders, want %d (file corruption / lost records)", len(res), n)
	}
	for _, o := range res {
		if o.State != StateFilled {
			t.Fatalf("order %s state=%s, want Filled", o.ClOrdID, o.State)
		}
		assertDecEq(t, o.CumQty, "10")
		if o.InFlight {
			t.Fatalf("order %s should not be in-flight", o.ClOrdID)
		}
	}
}

func mustNil(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Replay tolerates a duplicate FILL (same fillID) without double-counting — the
// dedup survives the journal round-trip.
func TestReplay_DuplicateFillDeduped(t *testing.T) {
	path := tmpJournal(t)
	j, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	id := ClientOrderID("s", "AAPL", "entry", 1)
	mustNil(t, j.AppendIntent(IntentRecord{ClOrdID: id, Strategy: "s", Symbol: "AAPL", Side: SideBuy, Qty: dec(t, "100"), Px: dec(t, "10")}))
	mustNil(t, j.AppendFill(id, "f1", dec(t, "40"), dec(t, "10")))
	mustNil(t, j.AppendFill(id, "f1", dec(t, "40"), dec(t, "10"))) // duplicate exec id
	mustNil(t, j.Close())

	res, err := Replay(path)
	if err != nil {
		t.Fatalf("replay with dup fill: %v", err)
	}
	assertDecEq(t, res[0].CumQty, "40") // not 80
	if res[0].State != StatePartiallyFilled {
		t.Fatalf("state=%s, want PartiallyFilled", res[0].State)
	}
}
