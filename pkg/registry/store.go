package registry

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"time"

	"github.com/lunc/mesh/pkg/crdt"
)

var (
	errInvalidSignature = errors.New("registry: invalid signature")
	errMissingOwnerKey  = errors.New("registry: missing owner public key")
	errMissingNodeKey   = errors.New("registry: missing node public key")
)

// Store maintains replicated metadata for backends and edges.
type Store struct {
	meta    *crdt.LWWMap[BackendID, BackendMeta]
	metaVV  *crdt.VersionVector
	nodes   *crdt.LWWMap[NodeID, NodeAdvert]
	nodeVV  *crdt.VersionVector
	metrics struct {
		sync.RWMutex
		items map[BackendID]timedMetric
	}
	metricsTTL time.Duration
}

type timedMetric struct {
	value      BackendMetrics
	receivedAt time.Time
}

// NewStore constructs a Store with the provided metrics TTL.
func NewStore(metricsTTL time.Duration) *Store {
	if metricsTTL <= 0 {
		metricsTTL = 10 * time.Second
	}

	s := &Store{
		meta:       crdt.NewLWWMap[BackendID, BackendMeta](),
		metaVV:     crdt.NewVersionVector(),
		nodes:      crdt.NewLWWMap[NodeID, NodeAdvert](),
		nodeVV:     crdt.NewVersionVector(),
		metricsTTL: metricsTTL,
	}
	s.metrics.items = make(map[BackendID]timedMetric)
	return s
}

// ApplyBackendMeta merges a BackendMeta announcement into the CRDT after verifying its signature.
func (s *Store) ApplyBackendMeta(ctx context.Context, meta BackendMeta) (bool, error) {
	if len(meta.OwnerPK) != ed25519.PublicKeySize {
		return false, errMissingOwnerKey
	}

	encoded, err := CanonicalBackendMeta(meta)
	if err != nil {
		return false, err
	}

	if !ed25519.Verify(ed25519.PublicKey(meta.OwnerPK), encoded, meta.Sig) {
		return false, errInvalidSignature
	}

	clock := crdt.Clock{Lamport: meta.Clock, UnixNano: meta.AddedAt * int64(time.Second)}
	changed := s.meta.Upsert(meta.ID, meta, clock)
	if changed {
		s.metaVV.Bump(string(meta.ID), meta.Clock)
	}
	return changed, nil
}

// ApplyNodeAdvert merges an edge advertisement into the CRDT after signature verification.
func (s *Store) ApplyNodeAdvert(ctx context.Context, advert NodeAdvert) (bool, error) {
	if len(advert.PubKey) != ed25519.PublicKeySize {
		return false, errMissingNodeKey
	}

	payload, err := CanonicalNodeAdvert(advert)
	if err != nil {
		return false, err
	}

	if !ed25519.Verify(ed25519.PublicKey(advert.PubKey), payload, advert.Sig) {
		return false, errInvalidSignature
	}

	clock := crdt.Clock{Lamport: advert.Clock, UnixNano: advert.AtUnix * int64(time.Second)}
	changed := s.nodes.Upsert(advert.ID, advert, clock)
	if changed {
		s.nodeVV.Bump(string(advert.ID), advert.Clock)
	}
	return changed, nil
}

// ApplyBackendMetrics stores ephemeral metrics with expiry.
func (s *Store) ApplyBackendMetrics(_ context.Context, m BackendMetrics) {
	s.metrics.Lock()
	s.metrics.items[m.ID] = timedMetric{value: m, receivedAt: time.Now()}
	s.metrics.Unlock()
}

// Prune removes expired metrics and tombstones.
func (s *Store) Prune() {
	s.metrics.Lock()
	for id, metric := range s.metrics.items {
		if time.Since(metric.receivedAt) > s.metricsTTL {
			delete(s.metrics.items, id)
		}
	}
	s.metrics.Unlock()

	s.meta.Cleanup(s.metricsTTL)
	s.nodes.Cleanup(s.metricsTTL)
}

// BackendSnapshot returns the current backend metadata and a version vector snapshot.
func (s *Store) BackendSnapshot() (map[BackendID]crdt.Entry[BackendMeta], map[string]uint64) {
	entries := s.meta.Snapshot()
	reduced := make(map[BackendID]crdt.Entry[BackendMeta], len(entries))
	for k, v := range entries {
		reduced[k] = v
	}
	return reduced, s.metaVV.Snapshot()
}

// NodeSnapshot returns the current set of node adverts and version vector snapshot.
func (s *Store) NodeSnapshot() (map[NodeID]crdt.Entry[NodeAdvert], map[string]uint64) {
	entries := s.nodes.Snapshot()
	reduced := make(map[NodeID]crdt.Entry[NodeAdvert], len(entries))
	for k, v := range entries {
		reduced[k] = v
	}
	return reduced, s.nodeVV.Snapshot()
}

// MergeBackendDelta merges incoming state from peers following anti-entropy.
func (s *Store) MergeBackendDelta(entries map[BackendID]crdt.Entry[BackendMeta], vv map[string]uint64) {
	if s.meta.Merge(entries) {
		for node, counter := range vv {
			s.metaVV.Bump(node, counter)
		}
	}
}

// MergeNodeDelta merges node adverts from peers.
func (s *Store) MergeNodeDelta(entries map[NodeID]crdt.Entry[NodeAdvert], vv map[string]uint64) {
	if s.nodes.Merge(entries) {
		for node, counter := range vv {
			s.nodeVV.Bump(node, counter)
		}
	}
}

// LatestMetrics returns a copy of current backend metrics.
func (s *Store) LatestMetrics() map[BackendID]BackendMetrics {
	s.metrics.RLock()
	defer s.metrics.RUnlock()

	out := make(map[BackendID]BackendMetrics, len(s.metrics.items))
	for id, metric := range s.metrics.items {
		out[id] = metric.value
	}
	return out
}

// BackendVersionVector returns a snapshot of the backend CRDT version vector.
func (s *Store) BackendVersionVector() map[string]uint64 {
	return s.metaVV.Snapshot()
}

// NodeVersionVector returns a snapshot of the node advert CRDT version vector.
func (s *Store) NodeVersionVector() map[string]uint64 {
	return s.nodeVV.Snapshot()
}
