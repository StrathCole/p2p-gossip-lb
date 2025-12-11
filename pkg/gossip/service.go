package gossip

import (
	"context"
	"errors"
	"fmt"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/fxamacker/cbor/v2"
	"github.com/libp2p/go-libp2p"
	mplex "github.com/libp2p/go-libp2p-mplex"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	"github.com/libp2p/go-libp2p/p2p/transport/websocket"
	"github.com/multiformats/go-multiaddr"
	"go.uber.org/zap"

	"github.com/lunc/mesh/pkg/crdt"
	"github.com/lunc/mesh/pkg/registry"
)

const (
	TopicBackendsMeta    = "mesh/v1/backends/meta"
	TopicBackendsMetrics = "mesh/v1/backends/metrics"
	TopicNodeAdverts     = "mesh/v1/nodes/adverts"
)

var syncProtocolID = protocol.ID("/mesh/registry/1.0.0")

var (
	errNoIdentity = errors.New("gossip: identity key required")
)

type Config struct {
	ListenAddrs   []string
	Bootstrap     []string
	DataDir       string
	IdentityKey   crypto.PrivKey
	SyncInterval  time.Duration
	PruneInterval time.Duration
}

type Service struct {
	host          host.Host
	pubsub        *pubsub.PubSub
	topics        map[string]*pubsub.Topic
	store         *registry.Store
	log           *zap.Logger
	cancel        context.CancelFunc
	db            *badger.DB
	key           crypto.PrivKey
	subCancel     []context.CancelFunc
	syncInterval  time.Duration
	pruneInterval time.Duration
}

type syncRequest struct {
	MetaVV map[string]uint64 `cbor:"meta_vv"`
	NodeVV map[string]uint64 `cbor:"node_vv"`
}

type syncResponse struct {
	MetaEntries []backendSyncEntry `cbor:"meta,omitempty"`
	NodeEntries []nodeSyncEntry    `cbor:"nodes,omitempty"`
	MetaVV      map[string]uint64  `cbor:"meta_vv"`
	NodeVV      map[string]uint64  `cbor:"node_vv"`
}

type backendSyncEntry struct {
	ID    registry.BackendID   `cbor:"id"`
	Meta  registry.BackendMeta `cbor:"meta,omitempty"`
	Clock syncClock            `cbor:"clock"`
	Tomb  bool                 `cbor:"tomb"`
}

type nodeSyncEntry struct {
	ID     registry.NodeID     `cbor:"id"`
	Advert registry.NodeAdvert `cbor:"advert,omitempty"`
	Clock  syncClock           `cbor:"clock"`
	Tomb   bool                `cbor:"tomb"`
}

type syncClock struct {
	Lamport  uint64 `cbor:"lamport"`
	UnixNano int64  `cbor:"unix"`
}

func NewService(ctx context.Context, cfg Config, store *registry.Store, log *zap.Logger) (*Service, error) {
	if cfg.IdentityKey == nil {
		return nil, errNoIdentity
	}

	syncInterval := cfg.SyncInterval
	if syncInterval <= 0 {
		syncInterval = 10 * time.Second
	}

	pruneInterval := cfg.PruneInterval
	if pruneInterval <= 0 {
		pruneInterval = 30 * time.Second
	}

	options := []libp2p.Option{
		libp2p.ListenAddrStrings(cfg.ListenAddrs...),
		libp2p.Identity(cfg.IdentityKey),
		libp2p.Security(noise.ID, noise.New),
		libp2p.Muxer(mplex.ID, mplex.DefaultTransport),
		libp2p.Transport(tcp.NewTCPTransport),
		libp2p.Transport(websocket.New),
		libp2p.EnableNATService(),
	}

	h, err := libp2p.New(options...)
	if err != nil {
		return nil, fmt.Errorf("gossip: create host: %w", err)
	}

	ps, err := pubsub.NewGossipSub(ctx, h, pubsub.WithMessageSigning(true), pubsub.WithStrictSignatureVerification(true))
	if err != nil {
		return nil, fmt.Errorf("gossip: gossip sub: %w", err)
	}

	childCtx, cancel := context.WithCancel(ctx)

	svc := &Service{
		host:          h,
		pubsub:        ps,
		topics:        make(map[string]*pubsub.Topic),
		store:         store,
		log:           log,
		cancel:        cancel,
		key:           cfg.IdentityKey,
		syncInterval:  syncInterval,
		pruneInterval: pruneInterval,
	}

	if err := svc.registerValidators(); err != nil {
		cancel()
		return nil, fmt.Errorf("gossip: register validators: %w", err)
	}

	if cfg.DataDir != "" {
		db, err := badger.Open(badger.DefaultOptions(cfg.DataDir))
		if err != nil {
			cancel()
			return nil, fmt.Errorf("gossip: open badger: %w", err)
		}
		svc.db = db
	}

	for _, topic := range []string{TopicBackendsMeta, TopicBackendsMetrics, TopicNodeAdverts} {
		topicHandle, err := ps.Join(topic)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("gossip: subscribe %s: %w", topic, err)
		}

		svc.topics[topic] = topicHandle

		sub, err := topicHandle.Subscribe()
		if err != nil {
			cancel()
			return nil, fmt.Errorf("gossip: subscribe %s: %w", topic, err)
		}

		loopCtx, loopCancel := context.WithCancel(childCtx)
		svc.subCancel = append(svc.subCancel, loopCancel)
		go svc.consume(loopCtx, topic, sub)
	}

	svc.host.SetStreamHandler(syncProtocolID, svc.handleSyncStream)
	svc.startAntiEntropy(childCtx)
	svc.startMaintenance(childCtx)

	// Add network notifier to log peer connections
	svc.host.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(n network.Network, c network.Conn) {
			svc.log.Info("peer connected",
				zap.String("peer", c.RemotePeer().String()),
				zap.String("direction", c.Stat().Direction.String()),
			)
		},
		DisconnectedF: func(n network.Network, c network.Conn) {
			svc.log.Info("peer disconnected", zap.String("peer", c.RemotePeer().String()))
		},
	})

	log.Info("gossip service started",
		zap.String("peer_id", h.ID().String()),
		zap.Strings("listen_addrs", cfg.ListenAddrs),
		zap.Int("bootstrap_count", len(cfg.Bootstrap)),
	)

	if err := svc.connectBootstraps(ctx, cfg.Bootstrap); err != nil {
		log.Warn("bootstrapping failed", zap.Error(err))
	}

	return svc, nil
}

func (s *Service) consume(ctx context.Context, topic string, sub *pubsub.Subscription) {
	for {
		msg, err := sub.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("gossip subscription error", zap.String("topic", topic), zap.Error(err))
			continue
		}

		s.log.Debug("received gossip message",
			zap.String("topic", topic),
			zap.String("from", msg.ReceivedFrom.String()),
			zap.Int("size", len(msg.Data)),
		)

		env, err := decodeEnvelope(msg.Data)
		if err != nil {
			s.log.Warn("dropping message: decode failed", zap.String("topic", topic), zap.Error(err))
			continue
		}

		if env.Topic != topic {
			s.log.Warn("dropping message: topic mismatch", zap.String("expected", topic), zap.String("got", env.Topic))
			continue
		}

		if err := verifyEnvelope(msg.ReceivedFrom, env); err != nil {
			s.log.Warn("dropping message: signature verification failed", zap.String("topic", topic), zap.Error(err))
			continue
		}

		s.handlePayload(ctx, topic, env.Payload)
	}
}

func (s *Service) handlePayload(ctx context.Context, topic string, payload []byte) {
	switch topic {
	case TopicBackendsMeta:
		var meta registry.BackendMeta
		if err := cbor.Unmarshal(payload, &meta); err != nil {
			s.log.Warn("invalid backend meta", zap.Error(err))
			return
		}
		s.log.Info("received backend meta via gossip",
			zap.String("backend_id", string(meta.ID)),
			zap.String("chain_id", string(meta.ChainID)),
		)
		if _, err := s.store.ApplyBackendMeta(ctx, meta); err != nil {
			s.log.Warn("failed to apply backend meta", zap.Error(err))
		}
	case TopicBackendsMetrics:
		var metrics registry.BackendMetrics
		if err := cbor.Unmarshal(payload, &metrics); err != nil {
			s.log.Warn("invalid backend metrics", zap.Error(err))
			return
		}
		s.log.Info("received backend metrics via gossip",
			zap.String("backend_id", string(metrics.ID)),
			zap.Int64("height", metrics.Height),
			zap.String("health", metrics.Health),
		)
		s.store.ApplyBackendMetrics(ctx, metrics)
	case TopicNodeAdverts:
		var advert registry.NodeAdvert
		if err := cbor.Unmarshal(payload, &advert); err != nil {
			s.log.Warn("invalid node advert", zap.Error(err))
			return
		}
		if _, err := s.store.ApplyNodeAdvert(ctx, advert); err != nil {
			s.log.Warn("failed to apply node advert", zap.Error(err))
		}
	}
}

func (s *Service) Publish(ctx context.Context, topic string, payload []byte, clock uint64) error {
	if topic != TopicBackendsMeta && topic != TopicBackendsMetrics && topic != TopicNodeAdverts {
		return fmt.Errorf("gossip: unknown topic %s", topic)
	}

	encoded, err := encodeEnvelope(topic, payload, clock, s.key)
	if err != nil {
		return err
	}

	t, ok := s.topics[topic]
	if !ok {
		return fmt.Errorf("gossip: topic %s not initialised", topic)
	}

	peers := s.host.Network().Peers()
	s.log.Debug("publishing message",
		zap.String("topic", topic),
		zap.Uint64("clock", clock),
		zap.Int("connected_peers", len(peers)),
	)

	return t.Publish(ctx, encoded)
}

// PublishBackendMeta broadcasts backend metadata and updates the local store.
func (s *Service) PublishBackendMeta(ctx context.Context, meta registry.BackendMeta) error {
	buf, err := cbor.Marshal(meta)
	if err != nil {
		return err
	}
	if _, err := s.store.ApplyBackendMeta(ctx, meta); err != nil {
		s.log.Warn("failed to apply local backend meta", zap.Error(err))
	}
	return s.Publish(ctx, TopicBackendsMeta, buf, meta.Clock)
}

// PublishBackendMetrics broadcasts latest backend metrics.
func (s *Service) PublishBackendMetrics(ctx context.Context, metrics registry.BackendMetrics) error {
	buf, err := cbor.Marshal(metrics)
	if err != nil {
		return err
	}
	s.store.ApplyBackendMetrics(ctx, metrics)
	return s.Publish(ctx, TopicBackendsMetrics, buf, uint64(metrics.AtUnix))
}

// PublishNodeAdvert announces edge availability for DNS service.
func (s *Service) PublishNodeAdvert(ctx context.Context, advert registry.NodeAdvert) error {
	buf, err := cbor.Marshal(advert)
	if err != nil {
		return err
	}
	changed, err := s.store.ApplyNodeAdvert(ctx, advert)
	if err != nil {
		s.log.Warn("failed to apply local node advert", zap.Error(err))
	} else {
		s.log.Info("published node advert",
			zap.String("node_id", string(advert.ID)),
			zap.Bool("ns_serving", advert.NSServing),
			zap.Int("ips", len(advert.IPs)),
			zap.Bool("changed", changed),
		)
	}
	return s.Publish(ctx, TopicNodeAdverts, buf, advert.Clock)
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.cancel()
	for _, cancel := range s.subCancel {
		cancel()
	}
	if s.db != nil {
		done := make(chan struct{})
		go func() {
			_ = s.db.Close()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	s.host.RemoveStreamHandler(syncProtocolID)
	return s.host.Close()
}

func (s *Service) connectBootstraps(ctx context.Context, peers []string) error {
	if len(peers) == 0 {
		s.log.Info("no bootstrap peers configured")
		return nil
	}
	s.log.Info("connecting to bootstrap peers", zap.Strings("peers", peers))
	for _, addr := range peers {
		ma, err := multiaddr.NewMultiaddr(addr)
		if err != nil {
			s.log.Warn("invalid bootstrap", zap.String("addr", addr), zap.Error(err))
			continue
		}
		info, err := peer.AddrInfoFromP2pAddr(ma)
		if err != nil {
			s.log.Warn("invalid bootstrap info", zap.Error(err))
			continue
		}
		if err := s.host.Connect(ctx, *info); err != nil {
			s.log.Warn("bootstrap connect failed", zap.String("peer", info.ID.String()), zap.Error(err))
		} else {
			s.log.Info("connected to bootstrap peer", zap.String("peer", info.ID.String()))
		}
	}
	return nil
}

func (s *Service) registerValidators() error {
	validatorTopics := []string{TopicBackendsMeta, TopicBackendsMetrics, TopicNodeAdverts}
	for _, topic := range validatorTopics {
		topic := topic
		if err := s.pubsub.RegisterTopicValidator(topic, func(ctx context.Context, id peer.ID, msg *pubsub.Message) bool {
			env, err := decodeEnvelope(msg.Data)
			if err != nil {
				s.log.Debug("dropping gossip before propagation", zap.String("topic", topic), zap.String("reason", "decode"), zap.Error(err))
				return false
			}
			if env.Topic != topic {
				s.log.Debug("dropping gossip before propagation", zap.String("topic", topic), zap.String("reason", "topic mismatch"))
				return false
			}
			if err := verifyEnvelope(id, env); err != nil {
				s.log.Debug("dropping gossip before propagation", zap.String("topic", topic), zap.String("reason", "signature"), zap.Error(err))
				return false
			}
			return true
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) startAntiEntropy(ctx context.Context) {
	if s.store == nil {
		return
	}
	interval := s.syncInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runSyncRound(ctx)
			}
		}
	}()
}

func (s *Service) startMaintenance(ctx context.Context) {
	if s.store == nil {
		return
	}
	interval := s.pruneInterval
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.store.Prune()
			}
		}
	}()
}

func (s *Service) runSyncRound(parent context.Context) {
	peers := s.host.Network().Peers()
	if len(peers) == 0 {
		return
	}
	for _, pid := range peers {
		if pid == s.host.ID() {
			continue
		}
		if err := s.syncWithPeer(parent, pid); err != nil {
			s.log.Debug("anti-entropy sync failed", zap.String("peer", pid.String()), zap.Error(err))
		}
	}
}

func (s *Service) syncWithPeer(parent context.Context, pid peer.ID) error {
	ctx, cancel := context.WithTimeout(parent, s.syncInterval+5*time.Second)
	defer cancel()

	stream, err := s.host.NewStream(ctx, pid, syncProtocolID)
	if err != nil {
		return err
	}
	defer func() {
		_ = stream.Close()
	}()

	request := syncRequest{
		MetaVV: s.store.BackendVersionVector(),
		NodeVV: s.store.NodeVersionVector(),
	}

	_ = stream.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := cbor.NewEncoder(stream).Encode(request); err != nil {
		_ = stream.Reset()
		return err
	}
	if err := stream.CloseWrite(); err != nil {
		return err
	}

	_ = stream.SetReadDeadline(time.Now().Add(15 * time.Second))
	var response syncResponse
	if err := cbor.NewDecoder(stream).Decode(&response); err != nil {
		_ = stream.Reset()
		return err
	}

	s.mergeSyncResponse(response)
	return nil
}

func (s *Service) handleSyncStream(stream network.Stream) {
	defer func() {
		_ = stream.Close()
	}()

	_ = stream.SetReadDeadline(time.Now().Add(15 * time.Second))
	var req syncRequest
	if err := cbor.NewDecoder(stream).Decode(&req); err != nil {
		s.log.Debug("anti-entropy request decode failed", zap.Error(err))
		_ = stream.Reset()
		return
	}
	_ = stream.CloseRead()

	metaSnapshot, metaVV := s.store.BackendSnapshot()
	nodeSnapshot, nodeVV := s.store.NodeSnapshot()
	resp := syncResponse{
		MetaEntries: toBackendSyncEntries(metaSnapshot),
		NodeEntries: toNodeSyncEntries(nodeSnapshot),
		MetaVV:      metaVV,
		NodeVV:      nodeVV,
	}

	_ = stream.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if err := cbor.NewEncoder(stream).Encode(resp); err != nil {
		s.log.Debug("anti-entropy response encode failed", zap.Error(err))
		_ = stream.Reset()
		return
	}
	if err := stream.CloseWrite(); err != nil {
		s.log.Debug("anti-entropy close write failed", zap.Error(err))
	}
}

func (s *Service) mergeSyncResponse(resp syncResponse) {
	if len(resp.MetaEntries) > 0 || len(resp.NodeEntries) > 0 {
		s.log.Info("merging anti-entropy response",
			zap.Int("meta_entries", len(resp.MetaEntries)),
			zap.Int("node_entries", len(resp.NodeEntries)),
		)
	}
	if len(resp.MetaEntries) > 0 {
		s.store.MergeBackendDelta(fromBackendSyncEntries(resp.MetaEntries), resp.MetaVV)
	}
	if len(resp.NodeEntries) > 0 {
		s.store.MergeNodeDelta(fromNodeSyncEntries(resp.NodeEntries), resp.NodeVV)
	}
}

type envelope struct {
	Topic     string `cbor:"topic"`
	Clock     uint64 `cbor:"clock"`
	Time      int64  `cbor:"time"`
	Payload   []byte `cbor:"payload"`
	PubKey    []byte `cbor:"pub"`
	Signature []byte `cbor:"sig"`
}

func encodeEnvelope(topic string, payload []byte, clock uint64, key crypto.PrivKey) ([]byte, error) {
	pub, err := key.GetPublic().Raw()
	if err != nil {
		return nil, fmt.Errorf("envelope: extract public key: %w", err)
	}

	env := envelope{
		Topic:   topic,
		Clock:   clock,
		Time:    time.Now().Unix(),
		Payload: payload,
		PubKey:  pub,
	}

	toSign, err := canonicalEnvelope(env)
	if err != nil {
		return nil, err
	}

	sig, err := key.Sign(toSign)
	if err != nil {
		return nil, fmt.Errorf("envelope: sign: %w", err)
	}
	env.Signature = sig

	encMode, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		return nil, err
	}

	return encMode.Marshal(env)
}

func canonicalEnvelope(env envelope) ([]byte, error) {
	type signed struct {
		Topic   string `cbor:"topic"`
		Clock   uint64 `cbor:"clock"`
		Time    int64  `cbor:"time"`
		Payload []byte `cbor:"payload"`
		PubKey  []byte `cbor:"pub"`
	}

	msg := signed{
		Topic:   env.Topic,
		Clock:   env.Clock,
		Time:    env.Time,
		Payload: env.Payload,
		PubKey:  env.PubKey,
	}

	encMode, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		return nil, err
	}
	return encMode.Marshal(msg)
}

func decodeEnvelope(data []byte) (envelope, error) {
	var env envelope
	if err := cbor.Unmarshal(data, &env); err != nil {
		return envelope{}, err
	}
	return env, nil
}

func verifyEnvelope(from peer.ID, env envelope) error {
	pub, err := crypto.UnmarshalEd25519PublicKey(env.PubKey)
	if err != nil {
		return fmt.Errorf("envelope: unmarshal pubkey: %w", err)
	}

	peerID, err := peer.IDFromPublicKey(pub)
	if err != nil {
		return fmt.Errorf("envelope: peer id: %w", err)
	}

	if peerID != from {
		return fmt.Errorf("envelope: peer mismatch expected %s got %s", from, peerID)
	}

	toVerify, err := canonicalEnvelope(env)
	if err != nil {
		return err
	}

	if ok, err := pub.Verify(toVerify, env.Signature); err != nil || !ok {
		return fmt.Errorf("envelope: signature invalid")
	}

	return nil
}

func toBackendSyncEntries(entries map[registry.BackendID]crdt.Entry[registry.BackendMeta]) []backendSyncEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]backendSyncEntry, 0, len(entries))
	for id, entry := range entries {
		record := backendSyncEntry{
			ID:    id,
			Clock: syncClock{Lamport: entry.Clock.Lamport, UnixNano: entry.Clock.UnixNano},
			Tomb:  entry.Tombstone,
		}
		if !entry.Tombstone {
			record.Meta = entry.Value
		}
		out = append(out, record)
	}
	return out
}

func toNodeSyncEntries(entries map[registry.NodeID]crdt.Entry[registry.NodeAdvert]) []nodeSyncEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]nodeSyncEntry, 0, len(entries))
	for id, entry := range entries {
		record := nodeSyncEntry{
			ID:    id,
			Clock: syncClock{Lamport: entry.Clock.Lamport, UnixNano: entry.Clock.UnixNano},
			Tomb:  entry.Tombstone,
		}
		if !entry.Tombstone {
			record.Advert = entry.Value
		}
		out = append(out, record)
	}
	return out
}

func fromBackendSyncEntries(entries []backendSyncEntry) map[registry.BackendID]crdt.Entry[registry.BackendMeta] {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[registry.BackendID]crdt.Entry[registry.BackendMeta], len(entries))
	for _, entry := range entries {
		value := crdt.Entry[registry.BackendMeta]{
			Clock:     crdt.Clock{Lamport: entry.Clock.Lamport, UnixNano: entry.Clock.UnixNano},
			Tombstone: entry.Tomb,
		}
		if !entry.Tomb {
			value.Value = entry.Meta
		}
		out[entry.ID] = value
	}
	return out
}

func fromNodeSyncEntries(entries []nodeSyncEntry) map[registry.NodeID]crdt.Entry[registry.NodeAdvert] {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[registry.NodeID]crdt.Entry[registry.NodeAdvert], len(entries))
	for _, entry := range entries {
		value := crdt.Entry[registry.NodeAdvert]{
			Clock:     crdt.Clock{Lamport: entry.Clock.Lamport, UnixNano: entry.Clock.UnixNano},
			Tombstone: entry.Tomb,
		}
		if !entry.Tomb {
			value.Value = entry.Advert
		}
		out[entry.ID] = value
	}
	return out
}
