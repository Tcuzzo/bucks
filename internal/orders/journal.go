package orders

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// EventType names a write-ahead journal record kind.
type EventType string

const (
	// EventIntent is the durable "I am about to send this order" record. It MUST
	// be fsync'd before any network send so a crash between intent and ack is
	// recoverable (the order is surfaced as in-flight on replay).
	EventIntent EventType = "INTENT"
	// EventSent records that the order was handed to the broker.
	EventSent EventType = "SENT"
	// EventAck records the broker acknowledged the order.
	EventAck EventType = "ACK"
	// EventFill records an execution (partial or full).
	EventFill EventType = "FILL"
	// EventReject records the broker/risk rejected the order. Terminal.
	EventReject EventType = "REJECT"
	// EventCancel records the order was canceled. Terminal.
	EventCancel EventType = "CANCEL"
)

// Record is one line in the journal. It is serialized as a single JSON object
// per line (JSON Lines). Only the fields relevant to Type are populated; money
// fields use Decimal and serialize as JSON strings (no float).
type Record struct {
	Type    EventType `json:"type"`
	ClOrdID string    `json:"clOrdID"`

	// INTENT fields.
	Strategy string `json:"strategy,omitempty"`
	Symbol   string `json:"symbol,omitempty"`
	// Side has NO omitempty: Side is an int enum whose zero value SideBuy(0) is a
	// real, meaningful value. omitempty would drop the side of every BUY order from
	// the WAL — the durable record of intent — so a BUY would only replay correctly
	// by zero-value coincidence. Always serialize it.
	Side Side     `json:"side"`
	Qty  *Decimal `json:"qty,omitempty"`
	Px   *Decimal `json:"px,omitempty"`

	// FILL fields.
	FillID string `json:"fillID,omitempty"`

	// REJECT field.
	Reason string `json:"reason,omitempty"`
}

// IntentRecord is the typed payload for AppendIntent — the order intent that
// must be durable before the network send.
type IntentRecord struct {
	ClOrdID  string
	Strategy string
	Symbol   string
	Side     Side
	Qty      Decimal
	Px       Decimal
}

// syncer is the fsync seam. *os.File satisfies it via Sync(). Tests inject a
// recording implementation to assert AppendIntent synced before returning / before
// a "send" callback ran.
type syncer interface {
	Sync() error
}

// Journal is an append-only, fsync-on-write order log. It is safe for concurrent
// AppendX calls (guarded by a mutex); the file is never corrupted by interleaved
// writers and every record replays.
type Journal struct {
	mu   sync.Mutex
	f    *os.File
	w    *bufio.Writer
	sync syncer
}

// Open opens (creating if absent) the journal file at path in append mode.
func Open(path string) (*Journal, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("journal: open %s: %w", path, err)
	}
	return &Journal{
		f:    f,
		w:    bufio.NewWriter(f),
		sync: f,
	}, nil
}

// openWithSyncer is the test seam: it opens a journal but routes fsync calls
// through the supplied syncer instead of *os.File.Sync. It lets a test observe
// the exact moment AppendIntent syncs (to assert sync-before-send ordering)
// without depending on real disk fsync semantics. Production code uses Open.
func openWithSyncer(path string, s syncer) (*Journal, error) {
	j, err := Open(path)
	if err != nil {
		return nil, err
	}
	j.sync = s
	return j, nil
}

// Close flushes and closes the underlying file.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	if err := j.w.Flush(); err != nil {
		_ = j.f.Close()
		return fmt.Errorf("journal: flush on close: %w", err)
	}
	err := j.f.Close()
	j.f = nil
	return err
}

// appendRecord serializes rec as one JSON line, flushes the buffer to the OS,
// and fsyncs the file so the record is durable before the call returns. It holds
// the mutex so concurrent appends never interleave a partial line.
func (j *Journal) appendRecord(rec Record) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("journal: marshal %s: %w", rec.Type, err)
	}
	line = append(line, '\n')

	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return errors.New("journal: write on closed journal")
	}
	if _, err := j.w.Write(line); err != nil {
		return fmt.Errorf("journal: write: %w", err)
	}
	if err := j.w.Flush(); err != nil {
		return fmt.Errorf("journal: flush: %w", err)
	}
	if err := j.sync.Sync(); err != nil {
		return fmt.Errorf("journal: fsync: %w", err)
	}
	return nil
}

// AppendIntent durably records an order intent. It fsyncs BEFORE returning, so
// the INTENT is on stable storage before the caller performs any network send.
// This is the crash-recovery guarantee: an intent with no terminal event replays
// as in-flight.
func (j *Journal) AppendIntent(rec IntentRecord) error {
	qty := rec.Qty
	px := rec.Px
	return j.appendRecord(Record{
		Type:     EventIntent,
		ClOrdID:  rec.ClOrdID,
		Strategy: rec.Strategy,
		Symbol:   rec.Symbol,
		Side:     rec.Side,
		Qty:      &qty,
		Px:       &px,
	})
}

// AppendSent records SENT (durable).
func (j *Journal) AppendSent(clOrdID string) error {
	return j.appendRecord(Record{Type: EventSent, ClOrdID: clOrdID})
}

// AppendAck records ACK (durable).
func (j *Journal) AppendAck(clOrdID string) error {
	return j.appendRecord(Record{Type: EventAck, ClOrdID: clOrdID})
}

// AppendFill records a FILL execution (durable).
func (j *Journal) AppendFill(clOrdID, fillID string, qty, px Decimal) error {
	return j.appendRecord(Record{
		Type:    EventFill,
		ClOrdID: clOrdID,
		FillID:  fillID,
		Qty:     &qty,
		Px:      &px,
	})
}

// AppendReject records REJECT with a reason (durable, terminal).
func (j *Journal) AppendReject(clOrdID, reason string) error {
	return j.appendRecord(Record{Type: EventReject, ClOrdID: clOrdID, Reason: reason})
}

// AppendCancel records CANCEL (durable, terminal).
func (j *Journal) AppendCancel(clOrdID string) error {
	return j.appendRecord(Record{Type: EventCancel, ClOrdID: clOrdID})
}

// ReplayResult is one reconstructed order plus the crash-recovery flag.
type ReplayResult struct {
	*Order
	// InFlight is true when the order has an INTENT but no terminal event
	// (Filled / Canceled / Rejected) — the order may or may not have reached the
	// broker, so reconcile must resolve it before arming any strategy.
	InFlight bool
}

// Replay reconstructs order state from the journal at path. It returns one
// ReplayResult per ClOrdID, in first-seen order.
//
// Crash-safety: a truncated/corrupt TRAILING line (expected on a real crash mid-
// write) is skipped without error — only the last line may be partial because
// every completed append is fsync'd as a whole line. A malformed line that is NOT
// the last line indicates real corruption and returns an error.
//
// An order whose terminal event never landed is surfaced with InFlight=true.
func Replay(path string) ([]*ReplayResult, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // nothing journaled yet
		}
		return nil, fmt.Errorf("journal: open for replay %s: %w", path, err)
	}
	defer f.Close()

	byID := make(map[string]*ReplayResult)
	var order []string // first-seen order of ClOrdIDs

	r := bufio.NewReader(f)
	var lineNo int
	for {
		line, readErr := r.ReadBytes('\n')
		hasNewline := len(line) > 0 && line[len(line)-1] == '\n'
		if hasNewline {
			line = line[:len(line)-1]
		}

		if len(line) > 0 {
			lineNo++
			// A line WITHOUT a trailing newline at EOF is a partial last write
			// (a crash mid-append). Tolerate it: skip and stop.
			if !hasNewline && readErr == io.EOF {
				break
			}
			var rec Record
			if jerr := json.Unmarshal(line, &rec); jerr != nil {
				// Malformed but newline-terminated => real corruption, not a crash tail.
				return nil, fmt.Errorf("journal: corrupt record at line %d: %w", lineNo, jerr)
			}
			res, ok := byID[rec.ClOrdID]
			if !ok {
				res = &ReplayResult{}
				byID[rec.ClOrdID] = res
				order = append(order, rec.ClOrdID)
			}
			if applyErr := applyRecord(res, rec); applyErr != nil {
				return nil, fmt.Errorf("journal: replay line %d (%s): %w", lineNo, rec.Type, applyErr)
			}
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("journal: read at line %d: %w", lineNo, readErr)
		}
	}

	out := make([]*ReplayResult, 0, len(order))
	for _, id := range order {
		res := byID[id]
		if res.Order == nil {
			// Saw events for an order with no INTENT — corrupt/partial log.
			return nil, fmt.Errorf("journal: events for %q with no INTENT", id)
		}
		// In-flight = intent seen, no terminal event reached.
		res.InFlight = !res.Order.State.IsTerminal()
		out = append(out, res)
	}
	return out, nil
}

// applyRecord folds one record into the reconstructed order.
func applyRecord(res *ReplayResult, rec Record) error {
	switch rec.Type {
	case EventIntent:
		if rec.Qty == nil || rec.Px == nil {
			return errors.New("intent missing qty/px")
		}
		res.Order = NewOrder(rec.ClOrdID, rec.Strategy, rec.Symbol, rec.Side, *rec.Qty, *rec.Px)
	case EventSent, EventAck:
		if res.Order == nil {
			return fmt.Errorf("%s before INTENT", rec.Type)
		}
		// SENT/ACK do not change order accounting or state in this spine; they
		// are durability/observability markers.
	case EventFill:
		if res.Order == nil {
			return errors.New("FILL before INTENT")
		}
		if rec.Qty == nil || rec.Px == nil {
			return errors.New("fill missing qty/px")
		}
		if err := res.Order.ApplyFill(rec.FillID, *rec.Qty, *rec.Px); err != nil &&
			!errors.Is(err, ErrFillAlreadyApplied) {
			return err
		}
	case EventReject:
		if res.Order == nil {
			return errors.New("REJECT before INTENT")
		}
		if err := res.Order.Reject(); err != nil {
			return err
		}
	case EventCancel:
		if res.Order == nil {
			return errors.New("CANCEL before INTENT")
		}
		if err := res.Order.Cancel(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown event type %q", rec.Type)
	}
	return nil
}
