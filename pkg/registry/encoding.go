package registry

import (
	"github.com/fxamacker/cbor/v2"
)

// CanonicalBackendMeta encodes backend metadata deterministically for signing.
func CanonicalBackendMeta(meta BackendMeta) ([]byte, error) {
	type signed struct {
		ID       BackendID       `cbor:"id"`
		Host     string          `cbor:"host"`
		IPs      [][]byte        `cbor:"ips"`
		Country  string          `cbor:"country"`
		Region   string          `cbor:"region"`
		ChainID  ChainID         `cbor:"chain_id"`
		Caps     map[string]bool `cbor:"caps"`
		Archival bool            `cbor:"archival"`
		AddedAt  int64           `cbor:"added_at"`
		Clock    uint64          `cbor:"clock"`
		OwnerPK  []byte          `cbor:"owner_pk"`
	}

	ips := make([][]byte, 0, len(meta.IPs))
	for _, ip := range meta.IPs {
		cp := make([]byte, len(ip))
		copy(cp, ip)
		ips = append(ips, cp)
	}

	msg := signed{
		ID:       meta.ID,
		Host:     meta.Host,
		IPs:      ips,
		Country:  meta.Country,
		Region:   meta.Region,
		ChainID:  meta.ChainID,
		Caps:     meta.Caps,
		Archival: meta.Archival,
		AddedAt:  meta.AddedAt,
		Clock:    meta.Clock,
		OwnerPK:  meta.OwnerPK,
	}

	encMode, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		return nil, err
	}

	return encMode.Marshal(msg)
}

// CanonicalNodeAdvert encodes node adverts with canonical CBOR encoding.
func CanonicalNodeAdvert(advert NodeAdvert) ([]byte, error) {
	type signed struct {
		ID        NodeID   `cbor:"id"`
		NSServing bool     `cbor:"ns"`
		IPs       [][]byte `cbor:"ips"`
		Country   string   `cbor:"country"`
		Region    string   `cbor:"region"`
		AtUnix    int64    `cbor:"at_unix"`
		Clock     uint64   `cbor:"clock"`
		PubKey    []byte   `cbor:"pub_key"`
	}

	ips := make([][]byte, 0, len(advert.IPs))
	for _, ip := range advert.IPs {
		cp := make([]byte, len(ip))
		copy(cp, ip)
		ips = append(ips, cp)
	}

	msg := signed{
		ID:        advert.ID,
		NSServing: advert.NSServing,
		IPs:       ips,
		Country:   advert.Country,
		Region:    advert.Region,
		AtUnix:    advert.AtUnix,
		Clock:     advert.Clock,
		PubKey:    advert.PubKey,
	}

	encMode, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		return nil, err
	}

	return encMode.Marshal(msg)
}

// CanonicalBackendMetrics encodes backend metrics deterministically for signing.
func CanonicalBackendMetrics(metrics BackendMetrics) ([]byte, error) {
	type signed struct {
		ID         BackendID `cbor:"id"`
		AtUnix     int64     `cbor:"at_unix"`
		Height     int64     `cbor:"height"`
		CatchingUp bool      `cbor:"catching_up"`
		CPU        float32   `cbor:"cpu"`
		P95ms      float32   `cbor:"p95_ms"`
		RPS        float32   `cbor:"rps"`
		FailRate   float32   `cbor:"fail_rate"`
		RTTms      float32   `cbor:"rtt_ms"`
		Health     string    `cbor:"health"`
	}

	msg := signed{
		ID:         metrics.ID,
		AtUnix:     metrics.AtUnix,
		Height:     metrics.Height,
		CatchingUp: metrics.CatchingUp,
		CPU:        metrics.CPU,
		P95ms:      metrics.P95ms,
		RPS:        metrics.RPS,
		FailRate:   metrics.FailRate,
		RTTms:      metrics.RTTms,
		Health:     metrics.Health,
	}

	encMode, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		return nil, err
	}

	return encMode.Marshal(msg)
}
