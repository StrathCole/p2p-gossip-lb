package meshtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"
)

// ClientConfig builds a TLS configuration for mesh clients (edge→backend connections).
type ClientConfig struct {
	RootCAFile         string
	ClientCertFile     string
	ClientKeyFile      string
	InsecureSkipVerify bool
	MinVersion         uint16
}

// BuildClientTLSConfig creates a tls.Config for outbound mesh connections with mTLS.
func BuildClientTLSConfig(cfg ClientConfig) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}

	if cfg.MinVersion > 0 {
		tlsConfig.MinVersion = cfg.MinVersion
	}

	if cfg.RootCAFile != "" {
		caCert, err := os.ReadFile(cfg.RootCAFile)
		if err != nil {
			return nil, fmt.Errorf("tls: read root CA: %w", err)
		}
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("tls: invalid root CA cert")
		}
		tlsConfig.RootCAs = caCertPool
	}

	if cfg.ClientCertFile != "" && cfg.ClientKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.ClientCertFile, cfg.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("tls: load client cert: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	return tlsConfig, nil
}

// ServerConfig builds a TLS configuration for mesh servers (backend, edge ingress).
type ServerConfig struct {
	CertFile           string
	KeyFile            string
	ClientCAFile       string
	RequireClientCert  bool
	MinVersion         uint16
	MaxVersion         uint16
	CipherSuites       []uint16
	SessionTicketsOff  bool
	RenegotiationLevel tls.RenegotiationSupport
}

// BuildServerTLSConfig creates a tls.Config for inbound mesh connections with optional mTLS.
func BuildServerTLSConfig(cfg ServerConfig) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("tls: load server cert: %w", err)
	}

	tlsConfig := &tls.Config{
		Certificates:  []tls.Certificate{cert},
		MinVersion:    tls.VersionTLS12,
		Renegotiation: tls.RenegotiateNever,
	}

	if cfg.MinVersion > 0 {
		tlsConfig.MinVersion = cfg.MinVersion
	}
	if cfg.MaxVersion > 0 {
		tlsConfig.MaxVersion = cfg.MaxVersion
	}
	if len(cfg.CipherSuites) > 0 {
		tlsConfig.CipherSuites = cfg.CipherSuites
	}
	if cfg.RenegotiationLevel != 0 {
		tlsConfig.Renegotiation = cfg.RenegotiationLevel
	}

	if cfg.ClientCAFile != "" {
		caCert, err := os.ReadFile(cfg.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("tls: read client CA: %w", err)
		}
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("tls: invalid client CA cert")
		}
		tlsConfig.ClientCAs = caCertPool
		if cfg.RequireClientCert {
			tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
		} else {
			tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven
		}
	}

	return tlsConfig, nil
}

// ACMEConfig holds parameters for ACME DNS-01 or HTTP-01 certificate provisioning.
type ACMEConfig struct {
	Email       string
	Domains     []string
	CacheDir    string
	RenewBefore time.Duration
	Staging     bool
}

// DefaultACMEConfig returns sensible defaults for ACME certificate management.
func DefaultACMEConfig() ACMEConfig {
	return ACMEConfig{
		RenewBefore: 30 * 24 * time.Hour,
		CacheDir:    "/var/lib/mesh/acme",
		Staging:     false,
	}
}
