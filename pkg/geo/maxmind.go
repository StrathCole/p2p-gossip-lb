package geo

import (
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/oschwald/geoip2-golang"
)

// MaxMindProvider provides IP geolocation using MaxMind GeoIP2/GeoLite2 databases.
type MaxMindProvider struct {
	mu       sync.RWMutex
	cityDB   *geoip2.Reader
	asnDB    *geoip2.Reader
	fallback *StubProvider
}

// MaxMindConfig configures the MaxMind geo provider.
type MaxMindConfig struct {
	// CityDBPath is the path to the GeoIP2-City or GeoLite2-City database.
	CityDBPath string

	// ASNDBPath is the optional path to the GeoIP2-ASN or GeoLite2-ASN database.
	ASNDBPath string
}

// NewMaxMindProvider creates a new MaxMind-based geo provider.
// At minimum, a city database is required for geographic lookups.
func NewMaxMindProvider(cfg MaxMindConfig) (*MaxMindProvider, error) {
	if cfg.CityDBPath == "" {
		return nil, errors.New("geo: city database path required")
	}

	cityDB, err := geoip2.Open(cfg.CityDBPath)
	if err != nil {
		return nil, fmt.Errorf("geo: open city database: %w", err)
	}

	var asnDB *geoip2.Reader
	if cfg.ASNDBPath != "" {
		asnDB, err = geoip2.Open(cfg.ASNDBPath)
		if err != nil {
			cityDB.Close()
			return nil, fmt.Errorf("geo: open asn database: %w", err)
		}
	}

	return &MaxMindProvider{
		cityDB:   cityDB,
		asnDB:    asnDB,
		fallback: NewStubProvider(),
	}, nil
}

// Lookup returns the geographic location for an IP address.
func (p *MaxMindProvider) Lookup(ip net.IP) (*Location, error) {
	if ip == nil {
		return &Location{Country: "ZZ", Region: "unknown", City: "unknown"}, nil
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.cityDB == nil {
		return p.fallback.Lookup(ip)
	}

	record, err := p.cityDB.City(ip)
	if err != nil {
		// Fall back to stub provider for unknown IPs
		return p.fallback.Lookup(ip)
	}

	loc := &Location{
		Latitude:  record.Location.Latitude,
		Longitude: record.Location.Longitude,
		Country:   record.Country.IsoCode,
		City:      record.City.Names["en"],
	}

	// Get the most specific subdivision (region/state)
	if len(record.Subdivisions) > 0 {
		loc.Region = record.Subdivisions[0].IsoCode
	}

	// Look up ASN if database is available
	if p.asnDB != nil {
		asnRecord, err := p.asnDB.ASN(ip)
		if err == nil {
			loc.ASN = uint32(asnRecord.AutonomousSystemNumber)
		}
	}

	// Normalize empty values
	if loc.Country == "" {
		loc.Country = "ZZ"
	}
	if loc.Region == "" {
		loc.Region = "unknown"
	}
	if loc.City == "" {
		loc.City = "unknown"
	}

	return loc, nil
}

// Close closes the MaxMind database readers.
func (p *MaxMindProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	var errs []error

	if p.cityDB != nil {
		if err := p.cityDB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close city db: %w", err))
		}
		p.cityDB = nil
	}

	if p.asnDB != nil {
		if err := p.asnDB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close asn db: %w", err))
		}
		p.asnDB = nil
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// Reload reloads the MaxMind databases from disk.
// This can be used to update the databases without restarting the service.
func (p *MaxMindProvider) Reload(cfg MaxMindConfig) error {
	if cfg.CityDBPath == "" {
		return errors.New("geo: city database path required")
	}

	newCityDB, err := geoip2.Open(cfg.CityDBPath)
	if err != nil {
		return fmt.Errorf("geo: reload city database: %w", err)
	}

	var newASNDB *geoip2.Reader
	if cfg.ASNDBPath != "" {
		newASNDB, err = geoip2.Open(cfg.ASNDBPath)
		if err != nil {
			newCityDB.Close()
			return fmt.Errorf("geo: reload asn database: %w", err)
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Close old databases
	if p.cityDB != nil {
		p.cityDB.Close()
	}
	if p.asnDB != nil {
		p.asnDB.Close()
	}

	// Swap in new databases
	p.cityDB = newCityDB
	p.asnDB = newASNDB

	return nil
}
