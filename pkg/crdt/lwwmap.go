package crdt

import (
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Clock captures the lamport clock and wall time used for LWW conflict resolution.
type Clock struct {
	Lamport  uint64
	UnixNano int64
}

// Entry holds the value and metadata required to merge map state.
type Entry[V any] struct {
	Value     V
	Clock     Clock
	Tombstone bool
}

// LWWMap implements a last-writer-wins map with tombstone support.
type LWWMap[K comparable, V any] struct {
	mu    sync.RWMutex
	items map[K]Entry[V]
}

// NewLWWMap returns an empty LWWMap instance.
func NewLWWMap[K comparable, V any]() *LWWMap[K, V] {
	return &LWWMap[K, V]{
		items: make(map[K]Entry[V]),
	}
}

// Upsert inserts or updates the value if the provided clock dominates the existing entry.
func (m *LWWMap[K, V]) Upsert(key K, value V, clock Clock) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.items[key]
	if !ok || dominates(clock, entry.Clock) {
		m.items[key] = Entry[V]{Value: value, Clock: clock}
		return true
	}

	return false
}

// Delete marks the key as tombstoned if the provided clock dominates prior state.
func (m *LWWMap[K, V]) Delete(key K, clock Clock) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.items[key]
	if !ok || dominates(clock, entry.Clock) {
		m.items[key] = Entry[V]{Clock: clock, Tombstone: true}
		return true
	}

	return false
}

// Get returns the value if present and not tombstoned.
func (m *LWWMap[K, V]) Get(key K) (V, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, ok := m.items[key]
	if !ok || entry.Tombstone {
		var zero V
		return zero, false
	}
	return entry.Value, true
}

// Snapshot copies the internal map for merging or replication.
func (m *LWWMap[K, V]) Snapshot() map[K]Entry[V] {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cp := make(map[K]Entry[V], len(m.items))
	for k, v := range m.items {
		cp[k] = v
	}
	return cp
}

// Merge the provided entries into the map, returning true if any change occurred.
func (m *LWWMap[K, V]) Merge(entries map[K]Entry[V]) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	changed := false
	for key, incoming := range entries {
		current, ok := m.items[key]
		if !ok || dominates(incoming.Clock, current.Clock) {
			m.items[key] = incoming
			changed = true
		}
	}
	return changed
}

// Cleanup removes tombstoned entries older than the provided TTL.
func (m *LWWMap[K, V]) Cleanup(ttl time.Duration) int {
	if ttl <= 0 {
		return 0
	}

	threshold := time.Now().Add(-ttl).UnixNano()

	m.mu.Lock()
	defer m.mu.Unlock()

	removed := 0
	for key, entry := range m.items {
		if entry.Tombstone && entry.Clock.UnixNano <= threshold {
			delete(m.items, key)
			removed++
		}
	}
	return removed
}

// dominates returns true when lhs wins over rhs according to lamport, then unix time.
func dominates(lhs, rhs Clock) bool {
	if lhs.Lamport > rhs.Lamport {
		return true
	}
	if lhs.Lamport < rhs.Lamport {
		return false
	}
	return lhs.UnixNano >= rhs.UnixNano
}

// ErrConcurrentMerge indicates that version vectors are concurrent and require full sync.
var ErrConcurrentMerge = errors.New("crdt: concurrent version vectors")

// VersionVector tracks per-node counters for anti-entropy.
type VersionVector struct {
	mu sync.RWMutex
	vv map[string]uint64
}

// NewVersionVector returns a VersionVector initialised with an empty map.
func NewVersionVector() *VersionVector {
	return &VersionVector{vv: make(map[string]uint64)}
}

// Bump sets the counter for nodeID to at least counter.
func (v *VersionVector) Bump(nodeID string, counter uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if current, ok := v.vv[nodeID]; !ok || counter > current {
		v.vv[nodeID] = counter
	}
}

// Snapshot returns a copy for marshaling.
func (v *VersionVector) Snapshot() map[string]uint64 {
	v.mu.RLock()
	defer v.mu.RUnlock()

	out := make(map[string]uint64, len(v.vv))
	for k, c := range v.vv {
		out[k] = c
	}
	return out
}

// Comparison is the partial order relationship between two version vectors.
type Comparison int

const (
	// Concurrent indicates neither vector dominates the other.
	Concurrent Comparison = iota
	// Descendant indicates the left vector dominates the right.
	Descendant
	// Ancestor indicates the right vector dominates the left.
	Ancestor
	// Equal indicates the vectors are identical.
	Equal
)

// Compare returns the relationship between this vector and other.
func (v *VersionVector) Compare(other map[string]uint64) Comparison {
	self := v.Snapshot()

	selfGreater := false
	otherGreater := false

	keys := make(map[string]struct{})
	for k := range self {
		keys[k] = struct{}{}
	}
	for k := range other {
		keys[k] = struct{}{}
	}

	for k := range keys {
		s := self[k]
		o := other[k]
		if s > o {
			selfGreater = true
		} else if s < o {
			otherGreater = true
		}
		if selfGreater && otherGreater {
			return Concurrent
		}
	}

	switch {
	case selfGreater:
		return Descendant
	case otherGreater:
		return Ancestor
	default:
		return Equal
	}
}

// Digest provides a sorted list of node counters for deterministic hashing.
func (v *VersionVector) Digest() []string {
	snap := v.Snapshot()
	if len(snap) == 0 {
		return nil
	}
	keys := make([]string, 0, len(snap))
	for node, counter := range snap {
		keys = append(keys, node+"="+strconv.FormatUint(counter, 10))
	}
	sort.Strings(keys)
	return keys
}

// FilterBy returns entries with Lamport clocks greater than provided minLamport.
func (m *LWWMap[K, V]) FilterBy(minLamport uint64) map[K]Entry[V] {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make(map[K]Entry[V])
	for k, entry := range m.items {
		if entry.Clock.Lamport >= minLamport {
			out[k] = entry
		}
	}
	return out
}
