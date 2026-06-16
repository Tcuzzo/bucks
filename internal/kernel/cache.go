package kernel

import "sort"

// Cache is the in-memory store of the LATEST state objects the kernel and its
// handlers read: e.g. the latest quote per symbol, the latest order snapshot per
// clOrdID. It is a simple typed key/value map keyed by a composite (kind, key)
// pair so different state families never collide.
//
// THE WRITE-THEN-PUBLISH ORDERING (the core invariant):
//
//	A component that changes state MUST write the new value into the cache FIRST,
//	THEN publish the event announcing the change. Because dispatch is
//	single-threaded and the publish only enqueues, by the time any handler runs in
//	response to that event the cache already holds the fresh value. So a handler
//	reading the cache during event processing always sees the freshest state — it
//	can never observe a value older than the event that woke it.
//
// The Cache itself does not enforce ordering across the bus (it can't see the
// bus); the discipline lives at the call site and in PutThenPublish below, which
// makes the correct ordering the easy path. The dedicated test proves a handler
// triggered by an event reads the already-updated value.
//
// Determinism: Get/Put are O(1) and order-independent. Keys() returns sorted keys
// so any code that iterates the cache (snapshots, fingerprints) is deterministic
// rather than dependent on Go's randomized map iteration.
type Cache struct {
	values map[cacheKey]any
}

// cacheKey namespaces entries by a "kind" (e.g. "quote", "order") and a string
// key (e.g. the symbol or clOrdID) so two families with the same key string do
// not overwrite each other.
type cacheKey struct {
	kind string
	key  string
}

// NewCache returns an empty cache.
func NewCache() *Cache {
	return &Cache{values: make(map[cacheKey]any)}
}

// Put writes v as the latest value for (kind, key), overwriting any prior value.
// This is the "write" half of write-then-publish — call it BEFORE publishing the
// event that announces the change.
func (c *Cache) Put(kind, key string, v any) {
	c.values[cacheKey{kind: kind, key: key}] = v
}

// Get returns the latest value for (kind, key) and whether it was present.
func (c *Cache) Get(kind, key string) (any, bool) {
	v, ok := c.values[cacheKey{kind: kind, key: key}]
	return v, ok
}

// PutThenPublish encodes the write-then-publish discipline as a single call: it
// writes v into the cache and THEN enqueues e on the bus. Any handler that runs
// in response to e is guaranteed to see v in the cache, because the publish only
// enqueues and dispatch is single-threaded. Prefer this over a bare Put+Publish
// so the ordering can never be accidentally reversed at a call site.
func (c *Cache) PutThenPublish(b *Bus, kind, key string, v any, e Event) {
	c.Put(kind, key, v)
	b.Publish(e)
}

// Keys returns all (kind, key) pairs currently in the cache, sorted by kind then
// key, so snapshots and fingerprints over the cache are deterministic. Each entry
// is a [2]string of {kind, key}.
func (c *Cache) Keys() [][2]string {
	out := make([][2]string, 0, len(c.values))
	for k := range c.values {
		out = append(out, [2]string{k.kind, k.key})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}

// Len reports how many entries the cache holds.
func (c *Cache) Len() int {
	return len(c.values)
}
