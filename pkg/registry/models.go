package registry

import (
	"net"
)

// ChainID identifies a blockchain network.
type ChainID string

// BackendID uniquely identifies a backend node within the mesh.
type BackendID string

// NodeID identifies an edge or infrastructure node participating in the mesh control plane.
type NodeID string

// BackendMeta describes static metadata about a backend node.
type BackendMeta struct {
	ID       BackendID
	Host     string // host:port or ip:port
	IPs      []net.IP
	Country  string // ISO-3166-1 alpha-2
	Region   string // free-form region
	ChainID  ChainID
	Caps     map[string]bool
	Archival bool
	OwnerPK  []byte
	AddedAt  int64
	Sig      []byte
	Clock    uint64
}

// BackendMetrics captures live health and performance info for a backend node.
type BackendMetrics struct {
	ID         BackendID
	AtUnix     int64
	Height     int64
	CatchingUp bool
	CPU        float32
	P95ms      float32
	RPS        float32
	FailRate   float32
	RTTms      float32
	Health     string
	Sig        []byte
}

// NodeAdvert is published by edges to announce availability for DNS service and geo placement.
type NodeAdvert struct {
	ID        NodeID
	NSServing bool
	IPs       []net.IP
	Country   string
	Region    string
	AtUnix    int64
	PubKey    []byte
	Sig       []byte
	Clock     uint64
}
