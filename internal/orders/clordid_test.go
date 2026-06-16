package orders

import (
	"testing"
)

type idInput struct {
	strategy, symbol, intent string
	seq                      uint64
}

// Test determinism: the same inputs always produce the same ID, across many
// cases and repeated calls.
func TestClientOrderID_Deterministic(t *testing.T) {
	cases := []idInput{
		{"momentum", "AAPL", "entry", 0},
		{"momentum", "AAPL", "entry", 1},
		{"mean-reversion", "BTC/USD", "exit", 42},
		{"breakout", "TSLA", "scale-in", 9999999999},
		{"", "", "", 0},
		{"a", "b", "c", 18446744073709551615}, // max uint64
	}
	for _, c := range cases {
		first := ClientOrderID(c.strategy, c.symbol, c.intent, c.seq)
		for i := 0; i < 5; i++ {
			got := ClientOrderID(c.strategy, c.symbol, c.intent, c.seq)
			if got != first {
				t.Fatalf("non-deterministic for %+v: %q != %q", c, got, first)
			}
		}
	}
}

// Test distinctness: different field tuples produce different IDs, INCLUDING the
// delimiter-collision cases that a naive concatenation would conflate.
func TestClientOrderID_Distinct(t *testing.T) {
	cases := []struct {
		name string
		a, b idInput
	}{
		{"diff strategy", idInput{"momentum", "AAPL", "entry", 1}, idInput{"breakout", "AAPL", "entry", 1}},
		{"diff symbol", idInput{"momentum", "AAPL", "entry", 1}, idInput{"momentum", "MSFT", "entry", 1}},
		{"diff intent", idInput{"momentum", "AAPL", "entry", 1}, idInput{"momentum", "AAPL", "exit", 1}},
		{"diff seq", idInput{"momentum", "AAPL", "entry", 1}, idInput{"momentum", "AAPL", "entry", 2}},
		// Delimiter-collision: ("a","b","") vs ("ab","","") would collide under
		// naive "a|b|" style concatenation. Length-prefixing must keep them apart.
		{"boundary a,b vs ab,''", idInput{"a", "b", "", 0}, idInput{"ab", "", "", 0}},
		{"boundary '',ab vs a,b", idInput{"", "ab", "", 0}, idInput{"a", "b", "", 0}},
		{"boundary x,yz vs xy,z", idInput{"x", "yz", "q", 0}, idInput{"xy", "z", "q", 0}},
		// Moving a character across the symbol/intent boundary.
		{"boundary sym/intent", idInput{"s", "AAP", "Lentry", 0}, idInput{"s", "AAPL", "entry", 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ida := ClientOrderID(c.a.strategy, c.a.symbol, c.a.intent, c.a.seq)
			idb := ClientOrderID(c.b.strategy, c.b.symbol, c.b.intent, c.b.seq)
			if ida == idb {
				t.Fatalf("collision: %+v and %+v both => %q", c.a, c.b, ida)
			}
		})
	}
}

// Test format/charset: the ID is broker-safe (uppercase base32, no padding, no
// spaces) and bounded length.
func TestClientOrderID_Format(t *testing.T) {
	cases := []idInput{
		{"momentum", "AAPL", "entry", 0},
		{"x", "y z has spaces?!", "in/tent", 12345}, // dirty inputs
		{"", "", "", 0},
		{"unicode-π", "BTC/USD", "exit", 7},
	}
	const wantLen = (clOrdIDBytes*8 + 4) / 5
	for _, c := range cases {
		id := ClientOrderID(c.strategy, c.symbol, c.intent, c.seq)
		if len(id) != wantLen {
			t.Fatalf("len(%q)=%d, want %d for %+v", id, len(id), wantLen, c)
		}
		for _, r := range id {
			okUpper := r >= 'A' && r <= 'Z'
			okDigit := r >= '2' && r <= '7'
			if !okUpper && !okDigit {
				t.Fatalf("non-broker-safe char %q in %q (from %+v)", r, id, c)
			}
		}
		if !ValidClientOrderID(id) {
			t.Fatalf("ValidClientOrderID rejected its own output %q", id)
		}
	}
}

func TestValidClientOrderID_RejectsBad(t *testing.T) {
	bad := []string{
		"",
		"too short",
		"has a space in it nope nope nope!",
		"lowercaseaaaaaaaaaaaaaaaaaaaaaaaa",  // wrong charset (lowercase + wrong len)
		"0189AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", // contains 0,1,8,9 (not base32)
	}
	for _, s := range bad {
		if ValidClientOrderID(s) {
			t.Fatalf("ValidClientOrderID accepted bad id %q", s)
		}
	}
}
