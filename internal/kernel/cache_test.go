package kernel

import (
	"bucks/internal/orders"
	"testing"
)

// TestCachePutGet asserts basic typed Put/Get round-trips and namespacing by kind.
func TestCachePutGet(t *testing.T) {
	c := NewCache()

	if _, ok := c.Get("quote", "BTC"); ok {
		t.Fatalf("empty cache returned a value for missing key")
	}

	c.Put("quote", "BTC", mustDec("100.50"))
	c.Put("order", "BTC", "working") // same key string, different kind: no collision

	v, ok := c.Get("quote", "BTC")
	if !ok {
		t.Fatalf("quote/BTC missing after Put")
	}
	if px, isDec := v.(orders.Decimal); !isDec || px.Cmp(mustDec("100.50")) != 0 {
		t.Fatalf("quote/BTC = %v, want decimal 100.50", v)
	}

	ov, ok := c.Get("order", "BTC")
	if !ok || ov.(string) != "working" {
		t.Fatalf("order/BTC = %v, want \"working\" (kind namespacing broken)", ov)
	}

	// Overwrite returns the latest value.
	c.Put("quote", "BTC", mustDec("101.00"))
	v2, _ := c.Get("quote", "BTC")
	if v2.(orders.Decimal).Cmp(mustDec("101.00")) != 0 {
		t.Fatalf("after overwrite quote/BTC = %v, want 101.00", v2)
	}
}

// TestWriteThenPublishHandlerSeesFreshValue is the write-then-publish proof: a
// producer writes the NEW quote into the cache and THEN publishes the quote event;
// the subscribed handler, when it runs, must read the ALREADY-UPDATED value. If
// the publish ran before the write the handler would read the stale value and the
// assert would fail.
func TestWriteThenPublishHandlerSeesFreshValue(t *testing.T) {
	k := New()
	cache := k.Cache()

	// Seed a stale value so "saw the fresh value" is a real distinction.
	cache.Put("quote", "BTC", mustDec("100.00"))

	var observed orders.Decimal
	var observedOK bool
	k.Bus().Subscribe("quote", func(_ *Bus, e Event) {
		q := e.(quoteEvent)
		v, ok := cache.Get("quote", q.symbol)
		observedOK = ok
		if ok {
			observed = v.(orders.Decimal)
		}
	})

	// Producer: WRITE then PUBLISH via the helper that enforces ordering.
	newPx := mustDec("123.45")
	ev := quoteEvent{ts: 1, symbol: "BTC", px: newPx}
	cache.PutThenPublish(k.Bus(), "quote", "BTC", newPx, ev)
	k.Bus().drain()

	if !observedOK {
		t.Fatalf("handler did not find quote/BTC in cache")
	}
	if observed.Cmp(newPx) != 0 {
		t.Fatalf("handler read stale cache value %s, want fresh %s (write-then-publish violated)",
			observed.String(), newPx.String())
	}
}

// TestWriteThenPublishAcrossCascade asserts the ordering holds through a cascade:
// handler A writes a derived value to the cache then publishes a second event;
// handler B (on that second event) reads the derived value A just wrote.
func TestWriteThenPublishAcrossCascade(t *testing.T) {
	k := New()
	cache := k.Cache()

	k.Bus().Subscribe("quote", func(bus *Bus, e Event) {
		q := e.(quoteEvent)
		// Derive a "signal strength" and publish a downstream event, write-first.
		cache.Put("derived", q.symbol, q.px)
		bus.Publish(signalEvent{ts: q.ts, kind: q.symbol})
	})

	var sawDerived orders.Decimal
	var sawOK bool
	k.Bus().Subscribe("signal", func(_ *Bus, e Event) {
		sym := e.(signalEvent).kind
		v, ok := cache.Get("derived", sym)
		sawOK = ok
		if ok {
			sawDerived = v.(orders.Decimal)
		}
	})

	k.Submit(quoteEvent{ts: 5, symbol: "ETH", px: mustDec("2000.25")})

	if !sawOK || sawDerived.Cmp(mustDec("2000.25")) != 0 {
		t.Fatalf("cascade handler read derived=%v ok=%v, want 2000.25/true", sawDerived, sawOK)
	}
}

// TestCacheKeysSorted asserts Keys() returns a deterministic sorted order
// (kind then key), so any snapshot/fingerprint over the cache is reproducible.
func TestCacheKeysSorted(t *testing.T) {
	c := NewCache()
	c.Put("quote", "ETH", 1)
	c.Put("order", "ZZZ", 1)
	c.Put("quote", "BTC", 1)
	c.Put("order", "AAA", 1)

	got := c.Keys()
	want := [][2]string{
		{"order", "AAA"},
		{"order", "ZZZ"},
		{"quote", "BTC"},
		{"quote", "ETH"},
	}
	if len(got) != len(want) {
		t.Fatalf("Keys() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Keys()[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}
