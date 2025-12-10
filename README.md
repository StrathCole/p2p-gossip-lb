# Mesh Load Balancer

A decentralized geo-distributed load balancer for blockchain RPC/LCD/WebSocket/gRPC traffic using peer-to-peer gossip for control plane coordination.

## Overview

Mesh provides a distributed load balancing solution with:
- **Decentralized control plane**: LibP2P gossipsub with CRDT for backend registry
- **Smart routing**: Geo-aware selection with latency, load, and health scoring
- **Edge proxy**: HTTP/1.1, HTTP/2, and WebSocket reverse proxy with caching
- **Authoritative DNS**: CoreDNS plugin with EDNS Client Subnet support
- **High availability**: Circuit breakers, health checks, automatic failover

## Architecture

```
[Client] → [DNS] → [Edge Proxy] → [Backend RPC Node]
              ↓           ↓              ↓
         [Gossip Mesh]  [Gossip Mesh]  [Agent]
```

**Components:**
- **meshd**: Control plane daemon (gossip + registry)
- **meshdns**: Authoritative DNS server
- **meshproxy**: Edge reverse proxy
- **meshagent**: Backend agent (runs on RPC nodes)
- **meshctl**: CLI management tool
- **meshpki**: Certificate authority for mesh mTLS

## Quick Start

### Prerequisites

```bash
# Go 1.23 or later
go version

# For DNS server (optional)
# Requires ability to bind to port 53 (root or CAP_NET_BIND_SERVICE)
```

### Build

```bash
# Clone and build
git clone https://github.com/lunc/mesh.git
cd mesh
go build -o bin/ ./cmd/...

# Binaries will be in ./bin/
ls -l bin/
```

### Generate Identity Keys

Each node needs a LibP2P identity key:

```bash
./bin/meshctl genkeys > identity.key
# Save the private key shown
```

## Configuration

All tools use YAML configuration files. See `examples/` directory for complete examples.

### Edge Proxy (meshproxy)

**File: `edge.yaml`**

```yaml
# LibP2P identity
identity_key: "CAESQNExample..."  # Ed25519 private key (base64)

# Network
listen_addrs:
  - "/ip4/0.0.0.0/tcp/4001"       # LibP2P gossip
bootstrap_peers:
  - "/dns4/seed.mesh.example.com/tcp/4001/p2p/12D3KooW..."

# Proxy settings
listen_addr: ":443"
tls_cert: "/etc/mesh/tls/cert.pem"
tls_key: "/etc/mesh/tls/key.pem"

# Optional: ACME for automatic TLS
acme_domains:
  - "edge1.example.com"
acme_email: "admin@example.com"

# Rate limiting
rate_limit_rps: 1000     # Requests per second per IP
rate_limit_burst: 2000   # Burst capacity

# Cache
cache_size_mb: 512       # Total cache size
cache_ttl: 300           # TTL for cached responses (seconds)

# Node advertisement
country: "US"            # ISO 3166-1 alpha-2
region: "us-east"        # Arbitrary region name
advertise_ips:
  - "203.0.113.10"       # Public IP(s)

# Logging
log_level: "info"        # debug, info, warn, error
```

**Run:**

```bash
./bin/meshproxy --config edge.yaml
```

**Usage:**
- Proxies incoming HTTPS requests to selected backends
- Caches immutable height-based queries
- Rate limits by client IP
- Publishes NodeAdvert to gossip mesh
- Serves health check on `:8080/health`

### Backend Agent (meshagent)

**File: `agent.yaml`**

```yaml
# LibP2P identity
identity_key: "CAESQNExample..."

listen_addrs:
  - "/ip4/0.0.0.0/tcp/4001"
bootstrap_peers:
  - "/dns4/seed.mesh.example.com/tcp/4001/p2p/12D3KooW..."

# Backend configuration
backend_url: "http://localhost:26657"  # Your RPC endpoint
backend_type: "tendermint"             # tendermint, cosmos, etc.
owner_address: "terra1example..."      # Owner wallet address

# Health checks
health_check_interval: 10s
health_check_timeout: 5s
health_check_path: "/health"

# Metrics
metrics_interval: 30s                  # How often to publish metrics

# Node metadata
country: "US"
region: "us-east"
provider: "aws"                        # aws, gcp, azure, bare-metal
advertise_ips:
  - "203.0.113.20"

log_level: "info"
```

**Run:**

```bash
./bin/meshagent --config agent.yaml
```

**Usage:**
- Collects metrics from backend RPC node
- Publishes BackendMeta (once on startup)
- Publishes BackendMetrics (every 30s)
- Serves ownership verification on `:8081/.mesh/verify`
- Registers backend in distributed registry

### DNS Server (meshdns)

**File: `dns.yaml`**

```yaml
# DNS configuration
listen_addr: ":53"
zone: "mesh.example.com"

# LibP2P for registry sync
identity_key: "CAESQNExample..."
listen_addrs:
  - "/ip4/0.0.0.0/tcp/4001"
bootstrap_peers:
  - "/dns4/seed.mesh.example.com/tcp/4001/p2p/12D3KooW..."

# Rate limiting
rate_limit_qps: 10000    # Queries per second

# ECS (EDNS Client Subnet)
ecs_enabled: true
ecs_scope_prefix: 24     # /24 for IPv4

# Selection weights
geo_weight: 0.4          # Geographic proximity
latency_weight: 0.3      # Network latency
health_weight: 0.3       # Backend health

log_level: "info"
```

**Run:**

```bash
# Requires root or CAP_NET_BIND_SERVICE for port 53
sudo ./bin/meshdns --config dns.yaml
```

**Usage:**
- Serves authoritative DNS for configured zone
- Returns NS records for `ns1.mesh.example.com`, `ns2.mesh.example.com`
- Returns A/AAAA records for `rpc.<chain>.mesh.example.com`
- Uses ECS to select geographically close edges
- Implements DNS rate limiting

### Control Plane (meshd)

**File: `meshd.yaml`**

```yaml
identity_key: "CAESQNExample..."

listen_addrs:
  - "/ip4/0.0.0.0/tcp/4001"
  - "/ip6/::/tcp/4001"

# Bootstrap seeds (for initial nodes, leave empty)
bootstrap_peers: []

# Data directory for BadgerDB
data_dir: "/var/lib/mesh"

# Sync interval for anti-entropy
sync_interval: 10s

# Tombstone pruning
prune_interval: 1h

log_level: "info"
```

**Run:**

```bash
./bin/meshd --config meshd.yaml
```

**Usage:**
- Runs gossip service for mesh coordination
- Maintains CRDT registry of backends
- Performs anti-entropy sync every 10s
- Persists state to BadgerDB
- Exposes gRPC API on `:9000` (future)

### PKI Server (meshpki)

**File: `meshpki.yaml`**

```yaml
# Port for HTTP API
port: 8443

log_level: "info"
```

**Run:**

```bash
PORT=8443 ./bin/meshpki
```

**Usage:**
- Issues mesh certificates for mTLS
- Serves CA certificate on `/ca`
- Signs node certificates on `/sign?node_id=<id>`
- Certificates valid for 24 hours

**Example:**

```bash
# Get CA certificate
curl http://localhost:8443/ca > ca.pem

# Request node certificate
curl -X POST "http://localhost:8443/sign?node_id=backend-1" | jq -r '.cert' > node.pem
curl -X POST "http://localhost:8443/sign?node_id=backend-1" | jq -r '.key' > node.key
```

### CLI Tool (meshctl)

```bash
# Show cluster status
./bin/meshctl status

# Drain a backend node
./bin/meshctl drain backend-id-123

# Generate identity keypair
./bin/meshctl genkeys

# Manage allowlist
./bin/meshctl allowlist add node-id-456
./bin/meshctl allowlist remove node-id-789
./bin/meshctl allowlist list
```

## Deployment

### Docker

```dockerfile
FROM golang:1.23-alpine AS builder
WORKDIR /build
COPY . .
RUN go build -o meshproxy ./cmd/meshproxy

FROM alpine:latest
RUN apk --no-cache add ca-certificates
COPY --from=builder /build/meshproxy /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/meshproxy"]
```

### Kubernetes (Helm)

```bash
# Install edge proxies
helm install mesh-edge ./deploy/helm/mesh \
  --set edge.enabled=true \
  --set edge.replicaCount=3 \
  --set global.identityKey="..." \
  --set global.bootstrapPeers[0]="/dns4/seed.mesh/tcp/4001/p2p/..."

# Install DNS servers
helm install mesh-dns ./deploy/helm/mesh \
  --set dns.enabled=true \
  --set dns.replicaCount=2 \
  --set dns.config.zone="mesh.example.com"

# Install agent with backend
helm install mesh-agent ./deploy/helm/mesh \
  --set agent.enabled=true \
  --set agent.config.backendUrl="http://tendermint:26657"
```

### Bare Metal

```bash
# Install as systemd service
sudo cp bin/meshproxy /usr/local/bin/
sudo cp examples/edge.yaml /etc/mesh/

# Create systemd unit
cat > /etc/systemd/system/meshproxy.service <<EOF
[Unit]
Description=Mesh Edge Proxy
After=network.target

[Service]
Type=simple
User=mesh
ExecStart=/usr/local/bin/meshproxy --config /etc/mesh/edge.yaml
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now meshproxy
```

## Monitoring

### Prometheus Metrics

All components expose Prometheus metrics on `:8080/metrics`:

```yaml
# prometheus.yml
scrape_configs:
  - job_name: 'mesh-edge'
    static_configs:
      - targets: ['edge1:8080', 'edge2:8080', 'edge3:8080']
  
  - job_name: 'mesh-agent'
    static_configs:
      - targets: ['agent1:8080', 'agent2:8080']
```

**Key Metrics:**
- `mesh_requests_total` - Total requests by status code
- `mesh_request_duration_seconds` - Request latency histogram
- `mesh_cache_hits_total` - Cache hit/miss counters
- `mesh_backend_failures_total` - Backend failure count
- `mesh_active_connections` - Active client connections
- `mesh_gossip_messages_sent_total` - Gossip traffic
- `mesh_registry_entries` - CRDT registry size

### Logging

Structured JSON logs to stdout:

```json
{
  "level": "info",
  "ts": "2025-11-01T12:00:00Z",
  "caller": "proxy/proxy.go:123",
  "msg": "request handled",
  "chain": "terra-classic-1",
  "method": "GET",
  "path": "/cosmos/bank/v1beta1/balances/terra1...",
  "backend": "backend-us-east-1",
  "duration_ms": 45.2,
  "status": 200,
  "cache_hit": true
}
```

### Tracing

OpenTelemetry tracing to Jaeger/Tempo:

```yaml
# edge.yaml
tracing:
  enabled: true
  endpoint: "jaeger:4317"
  sample_rate: 0.05  # 5% sampling
```

## Advanced Configuration

### Selection Tuning

```yaml
# edge.yaml
selector:
  weights:
    rtt: 0.35        # Network latency
    p95: 0.25        # Query latency
    cpu: 0.15        # CPU utilization
    geo: 0.10        # Geographic distance
    fail_rate: 0.10  # Error rate
    height_lag: 0.05 # Block height lag
  
  height_delta: 2    # Max blocks behind leader
  softmax_temperature: 0.7
  stickiness_probability: 0.8
```

### Cache Configuration

```yaml
# edge.yaml
cache:
  enabled: true
  max_bytes: 8589934592      # 8 GB
  max_item_bytes: 8388608    # 8 MB per item
  ttl_heighted: "infinite"   # Cache height-based queries forever
  negative_ttl: 5            # Cache 404s for 5 seconds
```

### Rate Limiting

```yaml
# edge.yaml
rate_limit:
  per_ip_rps: 100
  burst: 200
  window: 60s

# Per-API key quotas (requires HMAC keys)
api_keys:
  - key: "key-premium-1"
    hmac_secret: "secret123"
    quota_rps: 1000
    quota_burst: 2000
```

### Circuit Breakers

```yaml
# edge.yaml
circuit_breaker:
  max_requests: 5        # Requests in half-open
  interval: 10s          # Reset interval
  timeout: 30s           # Open → half-open timeout
  failure_threshold: 0.2 # Trip at 20% error rate
  min_requests: 50       # Minimum requests before trip
```

## Troubleshooting

### Edge proxy not routing

```bash
# Check if gossip connected
curl http://localhost:8080/debug/peers

# Check registry
curl http://localhost:8080/debug/backends

# Check logs
journalctl -u meshproxy -f
```

### Backend not appearing in registry

```bash
# Verify ownership endpoint
curl http://backend:8081/.mesh/verify

# Check agent logs
./bin/meshagent --config agent.yaml --log-level debug

# Verify gossip connectivity
netstat -an | grep 4001
```

### DNS not resolving

```bash
# Test DNS directly
dig @localhost -p 53 rpc.terra-classic-1.mesh.example.com

# Check with ECS
dig @localhost -p 53 +subnet=1.2.3.4/24 rpc.terra-classic-1.mesh.example.com

# Verify zone
dig @localhost -p 53 NS mesh.example.com
```

### High latency

```bash
# Check cache hit rate
curl http://localhost:8080/metrics | grep cache_hits

# Check backend selection
# Enable debug logging to see selection scores
```

## Security

### Gossip Signing

All gossip messages are Ed25519 signed:

```bash
# Generate key
./bin/meshctl genkeys

# Use in config
identity_key: "CAESQN..."  # Private key
```

### Backend Ownership

Backends must prove ownership via HTTP-01:

```bash
# Agent serves challenge response on
# GET http://backend:8081/.mesh/verify
# Returns signed challenge proving ownership
```

### Mesh mTLS

Edge ↔ Backend communication uses mutual TLS:

```bash
# Issue certificates
curl -X POST "http://meshpki:8443/sign?node_id=edge-1" > edge-cert.json

# Configure edge
tls:
  mesh_cert: "/etc/mesh/edge-cert.pem"
  mesh_key: "/etc/mesh/edge-key.pem"
  mesh_ca: "/etc/mesh/ca.pem"
```

### Allowlist

Restrict which nodes can join:

```bash
./bin/meshctl allowlist add 12D3KooWExample...
./bin/meshctl allowlist add 12D3KooWAnother...

# In config
allowlist_enabled: true
allowlist_file: "/etc/mesh/allowlist.txt"
```

## Performance Tuning

### Edge Proxy

```yaml
# Increase connection limits
limits:
  max_idle_conns: 2000
  idle_conn_timeout: 90s
  max_conns_per_host: 100

# Tune read/write timeouts
timeouts:
  read_timeout: 30s
  write_timeout: 30s
  idle_timeout: 120s
```

### BadgerDB

```yaml
# meshd.yaml
badger:
  value_log_file_size: 1073741824  # 1 GB
  num_versions_to_keep: 1
  num_level_zero_tables: 5
```

### Gossip

```yaml
# Reduce gossip fanout for large meshes
gossip:
  d: 6           # Desired peers
  d_lo: 4        # Low watermark
  d_hi: 12       # High watermark
  heartbeat: 1s  # Heartbeat interval
```

## API Reference

### Edge Proxy Endpoints

```
GET  /health          - Health check (200 if healthy)
GET  /ready           - Readiness check
GET  /metrics         - Prometheus metrics
GET  /debug/peers     - Connected gossip peers (debug)
GET  /debug/backends  - Registered backends (debug)
```

### Agent Endpoints

```
GET  /.mesh/verify    - Ownership verification challenge
GET  /metrics         - Prometheus metrics
```

### PKI Endpoints

```
GET  /ca              - CA certificate (PEM)
POST /sign?node_id=X  - Sign node certificate (returns JSON)
```

## Development

### Running Tests

```bash
# Unit tests
go test ./...

# With race detector
go test -race ./...

# Integration tests
cd test/integration
docker-compose up -d
go test -v ./...
```

### Building Docker Images

```bash
# Build all images
docker build -t mesh/meshproxy:latest -f deployments/docker/meshproxy.Dockerfile .
docker build -t mesh/meshagent:latest -f deployments/docker/meshagent.Dockerfile .
docker build -t mesh/meshdns:latest -f deployments/docker/meshdns.Dockerfile .
```

## License

[Your License Here]

## Support

- Documentation: https://docs.mesh.example.com
- Issues: https://github.com/lunc/mesh/issues
- Discord: https://discord.gg/mesh
