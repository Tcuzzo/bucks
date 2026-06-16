package orders

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"strings"
)

// clOrdIDBytes is how many bytes of the SHA-256 digest we keep. 20 bytes -> 32
// base32 characters, well within every broker's client-order-ID length limit
// (Alpaca allows 128, FIX clOrdID is 64). 160 bits is collision-safe.
const clOrdIDBytes = 20

// base32NoPad encodes without the trailing '=' padding so the ID is a clean,
// fixed-charset token: uppercase A–Z and digits 2–7 only. No spaces, no symbols,
// broker-safe everywhere.
var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// ClientOrderID derives a deterministic, idempotent client-order-ID from the
// fields that define an order intent. The SAME inputs ALWAYS produce the SAME ID
// — it is reused verbatim on every retry so the broker dedupes duplicate sends
// (the random-UUID-on-restart duplicate-order bug, killed by construction).
//
// It is a pure function: no time, no rand, no UUID, no global state.
//
// Collision safety: each variable-length field is length-prefixed (8-byte
// big-endian length) before its bytes are written to the hash, and the seq is a
// fixed-width 8-byte integer. Length-prefixing makes the encoding unambiguous,
// so distinct field tuples can never hash the same pre-image. For example
// ("a","b",...) and ("ab","",...) produce different byte streams and thus
// different IDs.
func ClientOrderID(strategy, symbol, intent string, seq uint64) string {
	h := sha256.New()
	writeField(h, strategy)
	writeField(h, symbol)
	writeField(h, intent)

	var seqBuf [8]byte
	binary.BigEndian.PutUint64(seqBuf[:], seq)
	h.Write(seqBuf[:])

	digest := h.Sum(nil)
	return base32NoPad.EncodeToString(digest[:clOrdIDBytes])
}

// writeField writes a length-prefixed field into the hash so that field
// boundaries are unambiguous and "a"+"b" can never collide with "ab"+"".
func writeField(h interface{ Write([]byte) (int, error) }, s string) {
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
	h.Write(lenBuf[:])
	h.Write([]byte(s))
}

// ValidClientOrderID reports whether s has the exact shape ClientOrderID emits:
// a fixed-length, broker-safe base32 token (uppercase A–Z / digits 2–7 only,
// no padding or spaces). Used by tests and adapters to assert the contract.
func ValidClientOrderID(s string) bool {
	const want = (clOrdIDBytes*8 + 4) / 5 // base32 chars for clOrdIDBytes bytes
	if len(s) != want {
		return false
	}
	for _, r := range s {
		isUpper := r >= 'A' && r <= 'Z'
		isB32Digit := r >= '2' && r <= '7'
		if !isUpper && !isB32Digit {
			return false
		}
	}
	// Belt-and-suspenders: it must actually decode as our base32 alphabet.
	_, err := base32NoPad.DecodeString(strings.ToUpper(s))
	return err == nil
}
