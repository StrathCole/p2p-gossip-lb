package geo

import (
	"math"
	"net"
	"sync"
)

// Location represents a geographic location.
type Location struct {
	Latitude  float64
	Longitude float64
	Country   string
	Region    string
	City      string
	ASN       uint32
}

// GeoProvider provides IP geolocation services.
type GeoProvider interface {
	Lookup(ip net.IP) (*Location, error)
	Close() error
}

// StubProvider is a simple in-memory provider for testing/MVP.
type StubProvider struct {
	mu      sync.RWMutex
	mapping map[string]*Location
}

// NewStubProvider creates a stub provider with some hardcoded mappings.
func NewStubProvider() *StubProvider {
	return &StubProvider{
		mapping: map[string]*Location{
			// Example mappings - in production use MaxMind GeoIP2
			"8.8.8.8":              {Latitude: 37.4056, Longitude: -122.0775, Country: "US", Region: "CA", City: "Mountain View"},
			"1.1.1.1":              {Latitude: -33.8688, Longitude: 151.2093, Country: "AU", Region: "NSW", City: "Sydney"},
			"208.67.222.222":       {Latitude: 37.7749, Longitude: -122.4194, Country: "US", Region: "CA", City: "San Francisco"},
			"2001:4860:4860::8888": {Latitude: 37.4056, Longitude: -122.0775, Country: "US", Region: "CA", City: "Mountain View"},
		},
	}
}

// Lookup returns location for an IP address.
func (p *StubProvider) Lookup(ip net.IP) (*Location, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	key := ip.String()
	if loc, ok := p.mapping[key]; ok {
		return loc, nil
	}

	// Default to unknown location
	return &Location{
		Latitude:  0,
		Longitude: 0,
		Country:   "ZZ",
		Region:    "unknown",
		City:      "unknown",
	}, nil
}

// AddMapping adds a test mapping (for testing).
func (p *StubProvider) AddMapping(ipStr string, loc *Location) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mapping[ipStr] = loc
}

// Close closes the provider.
func (p *StubProvider) Close() error {
	return nil
}

// Distance calculates the great-circle distance in kilometers between two points.
// Uses the Haversine formula.
func Distance(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadius = 6371.0 // km

	// Convert to radians
	lat1Rad := lat1 * math.Pi / 180
	lat2Rad := lat2 * math.Pi / 180
	deltaLat := (lat2 - lat1) * math.Pi / 180
	deltaLon := (lon2 - lon1) * math.Pi / 180

	// Haversine formula
	a := math.Sin(deltaLat/2)*math.Sin(deltaLat/2) +
		math.Cos(lat1Rad)*math.Cos(lat2Rad)*
			math.Sin(deltaLon/2)*math.Sin(deltaLon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return earthRadius * c
}

// DistanceBetween calculates distance between two locations.
func DistanceBetween(a, b *Location) float64 {
	if a == nil || b == nil {
		return 0
	}
	return Distance(a.Latitude, a.Longitude, b.Latitude, b.Longitude)
}

// GeoPenalty calculates a penalty score based on distance.
// Returns 0.0 for same location, up to 1.0 for maximum distance.
func GeoPenalty(distanceKm float64) float64 {
	// Max distance on Earth is ~20,000 km (half circumference)
	const maxDistance = 20000.0

	if distanceKm <= 0 {
		return 0.0
	}
	if distanceKm >= maxDistance {
		return 1.0
	}

	// Linear scaling - can be replaced with exponential for more aggressive penalty
	return distanceKm / maxDistance
}

// CountryMatch returns a bonus if countries match.
func CountryMatch(a, b *Location) float64 {
	if a == nil || b == nil {
		return 0
	}
	if a.Country == b.Country && a.Country != "" && a.Country != "ZZ" {
		return 0.2 // 20% bonus for same country
	}
	return 0
}

// RegionMatch returns a bonus if regions match (stronger than country).
func RegionMatch(a, b *Location) float64 {
	if a == nil || b == nil {
		return 0
	}
	if a.Country == b.Country && a.Region == b.Region && a.Region != "" && a.Region != "unknown" {
		return 0.3 // 30% bonus for same region
	}
	return 0
}

// ASNMatch returns a bonus if ASNs match (indicating same network).
func ASNMatch(a, b *Location) float64 {
	if a == nil || b == nil {
		return 0
	}
	if a.ASN == b.ASN && a.ASN != 0 {
		return 0.4 // 40% bonus for same ASN (very close network-wise)
	}
	return 0
}

// TotalGeoScore combines distance penalty and match bonuses.
// Returns a score where lower is better.
func TotalGeoScore(client, backend *Location) float64 {
	dist := DistanceBetween(client, backend)
	penalty := GeoPenalty(dist)

	// Subtract bonuses from penalty
	bonus := CountryMatch(client, backend) + RegionMatch(client, backend) + ASNMatch(client, backend)

	score := penalty - bonus
	if score < 0 {
		score = 0
	}

	return score
}
