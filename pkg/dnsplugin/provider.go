package dnsplugin

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"net"
	"sort"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/zap"

	"github.com/lunc/mesh/pkg/geo"
	"github.com/lunc/mesh/pkg/registry"
)

// RegistryZoneView implements ZoneView backed by the shared registry store.
type RegistryZoneView struct {
	store       *registry.Store
	zone        string
	nsLabels    []string
	ttlNSA      uint32
	ttlService  uint32
	log         *zap.Logger
	serialCache atomic.Uint32
	geoProvider geo.GeoProvider
	geoWeight   float64 // Weight for geo-based scoring (0.0-1.0)
}

// RegistryZoneViewConfig describes the required inputs for creating the zone view.
type RegistryZoneViewConfig struct {
	Store       *registry.Store
	Zone        string
	NSLabels    []string
	TTLNSA      uint32
	TTLService  uint32
	Logger      *zap.Logger
	GeoProvider geo.GeoProvider
	GeoWeight   float64 // Weight for geo scoring (default: 0.5)
}

// NewRegistryZoneView builds a zone view for the DNS plugin.
func NewRegistryZoneView(cfg RegistryZoneViewConfig) (*RegistryZoneView, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("dnsplugin: registry store required")
	}
	if cfg.Zone == "" {
		return nil, fmt.Errorf("dnsplugin: zone required")
	}
	labels := cfg.NSLabels
	if len(labels) == 0 {
		labels = []string{"ns1." + cfg.Zone, "ns2." + cfg.Zone}
	}
	ttlNS := cfg.TTLNSA
	if ttlNS == 0 {
		ttlNS = 30
	}
	ttlSvc := cfg.TTLService
	if ttlSvc == 0 {
		ttlSvc = 20
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	geoWeight := cfg.GeoWeight
	if geoWeight <= 0 {
		geoWeight = 0.5 // Default geo weight
	}
	if geoWeight > 1.0 {
		geoWeight = 1.0
	}
	return &RegistryZoneView{
		store:       cfg.Store,
		zone:        dns.Fqdn(cfg.Zone),
		nsLabels:    labels,
		ttlNSA:      ttlNS,
		ttlService:  ttlSvc,
		log:         logger,
		geoProvider: cfg.GeoProvider,
		geoWeight:   geoWeight,
	}, nil
}

// EdgeRecords selects up to max records representing mesh edges suitable for the query.
// When a geo provider is configured and ECS IP is available, edges are sorted by
// geographic proximity to the client.
func (v *RegistryZoneView) EdgeRecords(ctx context.Context, chain registry.ChainID, max int, ecsIP net.IP) ([]dns.RR, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, _ := v.store.NodeSnapshot()
	v.log.Info("EdgeRecords called",
		zap.String("chain", string(chain)),
		zap.Int("node_entries", len(entries)),
		zap.String("ecs_ip", ecsIP.String()),
	)

	// Look up client location if geo provider is available
	var clientLoc *geo.Location
	if v.geoProvider != nil && ecsIP != nil {
		loc, err := v.geoProvider.Lookup(ecsIP)
		if err == nil && loc != nil {
			clientLoc = loc
			v.log.Debug("client geo lookup",
				zap.String("client_ip", ecsIP.String()),
				zap.String("country", loc.Country),
				zap.String("region", loc.Region),
				zap.Float64("lat", loc.Latitude),
				zap.Float64("lon", loc.Longitude),
			)
		}
	}

	type edge struct {
		rr       dns.RR
		ip       net.IP
		nodeID   registry.NodeID
		geoScore float64 // Lower is better (distance-based)
		hash     uint32  // For consistent ordering within same geo score
	}
	edges := make([]edge, 0, len(entries))
	seen := make(map[string]struct{})

	for id, entry := range entries {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		v.log.Debug("checking node entry",
			zap.String("id", string(id)),
			zap.Bool("tombstone", entry.Tombstone),
			zap.Bool("ns_serving", entry.Value.NSServing),
			zap.Int("ips", len(entry.Value.IPs)),
		)
		if entry.Tombstone {
			continue
		}
		advert := entry.Value
		if !advert.NSServing {
			continue
		}
		if len(advert.IPs) == 0 {
			continue
		}

		// Calculate geo score for this node
		var nodeGeoScore float64
		if clientLoc != nil && v.geoProvider != nil {
			nodeGeoScore = v.calculateNodeGeoScore(advert, clientLoc)
		}

		for _, ip := range advert.IPs {
			rr := toRR(ip, v.zone, v.ttlService)
			if rr == nil {
				continue
			}
			key := rr.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}

			// Use hash for consistent ordering among edges with similar geo scores
			hashScore := hashEdge(chain, advert.ID, ip, ecsIP)
			edges = append(edges, edge{
				rr:       rr,
				ip:       ip,
				nodeID:   id,
				geoScore: nodeGeoScore,
				hash:     hashScore,
			})
		}
	}

	if len(edges) == 0 {
		return nil, nil
	}

	// Sort by geo score (lower is better), then by hash for determinism
	sort.Slice(edges, func(i, j int) bool {
		// If geo scores differ significantly, use geo score
		if math.Abs(edges[i].geoScore-edges[j].geoScore) > 0.01 {
			return edges[i].geoScore < edges[j].geoScore
		}
		// Otherwise, use hash for consistent ordering
		return edges[i].hash < edges[j].hash
	})

	if max > 0 && len(edges) > max {
		edges = edges[:max]
	}

	records := make([]dns.RR, len(edges))
	for i := range edges {
		records[i] = edges[i].rr
		v.log.Debug("selected edge",
			zap.Int("rank", i),
			zap.String("ip", edges[i].ip.String()),
			zap.Float64("geo_score", edges[i].geoScore),
		)
	}
	return records, nil
}

// calculateNodeGeoScore calculates a geo-based score for a node.
// Lower scores are better (closer to client).
func (v *RegistryZoneView) calculateNodeGeoScore(advert registry.NodeAdvert, clientLoc *geo.Location) float64 {
	if v.geoProvider == nil || clientLoc == nil {
		return 0
	}

	// Try to get location from node's advertised IPs
	var nodeLoc *geo.Location
	for _, ip := range advert.IPs {
		loc, err := v.geoProvider.Lookup(ip)
		if err == nil && loc != nil && loc.Country != "ZZ" {
			nodeLoc = loc
			break
		}
	}

	// Fall back to node's declared country/region if IP lookup fails
	if nodeLoc == nil || nodeLoc.Country == "ZZ" {
		if advert.Country != "" {
			nodeLoc = countryToApproxLocation(advert.Country, advert.Region)
		}
	}

	if nodeLoc == nil {
		return 0.5 // Unknown location gets middle score
	}

	// Calculate total geo score (0.0 = same location, 1.0 = max distance)
	score := geo.TotalGeoScore(clientLoc, nodeLoc)

	v.log.Debug("node geo score",
		zap.String("node_country", nodeLoc.Country),
		zap.String("client_country", clientLoc.Country),
		zap.Float64("distance_km", geo.DistanceBetween(clientLoc, nodeLoc)),
		zap.Float64("score", score),
	)

	return score * v.geoWeight
}

// countryToApproxLocation returns an approximate location for a country code.
// This is used when IP geolocation fails but we have the node's declared country.
func countryToApproxLocation(country, region string) *geo.Location {
	// Approximate country centroids for major countries
	centroids := map[string]struct{ lat, lon float64 }{
		"US": {39.8283, -98.5795},
		"CA": {56.1304, -106.3468},
		"GB": {55.3781, -3.4360},
		"DE": {51.1657, 10.4515},
		"FR": {46.2276, 2.2137},
		"NL": {52.1326, 5.2913},
		"JP": {36.2048, 138.2529},
		"SG": {1.3521, 103.8198},
		"AU": {-25.2744, 133.7751},
		"BR": {-14.2350, -51.9253},
		"IN": {20.5937, 78.9629},
		"KR": {35.9078, 127.7669},
		"HK": {22.3193, 114.1694},
		"CH": {46.8182, 8.2275},
		"SE": {60.1282, 18.6435},
		"FI": {61.9241, 25.7482},
		"NO": {60.4720, 8.4689},
		"DK": {56.2639, 9.5018},
		"IE": {53.1424, -7.6921},
		"PL": {51.9194, 19.1451},
		"ES": {40.4637, -3.7492},
		"IT": {41.8719, 12.5674},
		"AT": {47.5162, 14.5501},
		"BE": {50.5039, 4.4699},
		"CZ": {49.8175, 15.4730},
		"PT": {39.3999, -8.2245},
		"RU": {61.5240, 105.3188},
		"CN": {35.8617, 104.1954},
		"TW": {23.6978, 120.9605},
		"MY": {4.2105, 101.9758},
		"ID": {-0.7893, 113.9213},
		"TH": {15.8700, 100.9925},
		"VN": {14.0583, 108.2772},
		"PH": {12.8797, 121.7740},
		"NZ": {-40.9006, 174.8860},
		"ZA": {-30.5595, 22.9375},
		"AE": {23.4241, 53.8478},
		"IL": {31.0461, 34.8516},
		"AR": {-38.4161, -63.6167},
		"CL": {-35.6751, -71.5430},
		"CO": {4.5709, -74.2973},
		"MX": {23.6345, -102.5528},
	}

	if c, ok := centroids[country]; ok {
		return &geo.Location{
			Latitude:  c.lat,
			Longitude: c.lon,
			Country:   country,
			Region:    region,
		}
	}

	return nil
}

// NSRecords returns the authoritative NS records for the zone.
func (v *RegistryZoneView) NSRecords(ctx context.Context) ([]dns.RR, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	records := make([]dns.RR, 0, len(v.nsLabels))
	for _, label := range v.nsLabels {
		fqdn := dns.Fqdn(label)
		records = append(records, &dns.NS{Hdr: dns.RR_Header{Name: v.zone, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: v.ttlNSA}, Ns: fqdn})
	}
	return records, nil
}

// ZoneSerial derives a zone serial based on the registry version vector.
func (v *RegistryZoneView) ZoneSerial() uint32 {
	snapshot := v.store.NodeVersionVector()
	var sum uint64
	for _, value := range snapshot {
		sum += value
	}
	if sum == 0 {
		now := uint32(time.Now().Unix())
		v.serialCache.Store(now)
		return now
	}
	serial := uint32(sum % uint64(mathMaxUint32))
	prev := v.serialCache.Load()
	if serial <= prev {
		if prev == mathMaxUint32 {
			serial = prev
		} else {
			serial = prev + 1
		}
	}
	v.serialCache.Store(serial)
	return serial
}

const mathMaxUint32 = ^uint32(0)

func hashEdge(chain registry.ChainID, id registry.NodeID, ip net.IP, ecs net.IP) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(chain))
	_, _ = h.Write([]byte(id))
	if ip != nil {
		_, _ = h.Write([]byte(ip))
	}
	if ecs != nil {
		_, _ = h.Write([]byte(ecs))
	}
	return h.Sum32()
}

func toRR(ip net.IP, zone string, ttl uint32) dns.RR {
	if ip == nil {
		return nil
	}
	header := dns.RR_Header{Name: zone, Class: dns.ClassINET, Ttl: ttl}
	if v4 := ip.To4(); v4 != nil {
		header.Rrtype = dns.TypeA
		return &dns.A{Hdr: header, A: v4}
	}
	if v6 := ip.To16(); v6 != nil {
		header.Rrtype = dns.TypeAAAA
		return &dns.AAAA{Hdr: header, AAAA: v6}
	}
	return nil
}
