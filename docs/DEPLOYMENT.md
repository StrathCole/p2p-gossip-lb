# Mesh Network Deployment Guide

This guide explains how to set up nodes to participate in the mesh load balancer network. The mesh consists of three types of nodes that can be deployed independently:

| Component | Purpose | When to Deploy |
|-----------|---------|----------------|
| **meshagent** | Monitors RPC backends and publishes metrics | On every RPC node |
| **meshproxy** | Edge proxy that routes client requests | At edge locations |
| **meshdns** | Authoritative DNS for service discovery | 2+ for redundancy |

## Prerequisites

- Go 1.23+ (for building from source)
- Linux server with public IP address
- Network connectivity between mesh nodes (TCP port 4001)
- For DNS: ability to bind port 53 (root or CAP_NET_BIND_SERVICE)

## Step 1: Build the Binaries

```bash
# Clone the repository
git clone https://github.com/lunc/mesh.git
cd mesh

# Build all components
go build -o bin/ ./cmd/...

# Verify binaries
ls -la bin/
# meshagent  meshctl  meshd  meshdns  meshpki  meshproxy
```

## Step 2: Generate Identity Keys

Each node in the mesh needs a unique LibP2P identity key. This key identifies the node and is used for signing gossip messages.

```bash
# Generate a new identity keypair
./bin/meshctl genkeys

# Output will be like:
# Peer ID: 12D3KooWExamplePeerID123456789...
# Private Key: CAESQNExamplePrivateKeyBase64...
```

**Save both values:**
- The **Peer ID** is needed for bootstrap configuration
- The **Private Key** goes in your config file as `gossip_key`

> ⚠️ **Security**: Keep the private key secure. Anyone with this key can impersonate your node.

## Step 3: Network Bootstrap

The mesh uses LibP2P for peer-to-peer communication. You need at least one known peer to bootstrap.

### First Node (Bootstrap Seed)

For the very first node, leave `bootstrap` empty. Other nodes will connect to you:

```yaml
mesh:
  gossip_key: "CAESQNYourPrivateKey..."
  listen_addrs:
    - "/ip4/0.0.0.0/tcp/4001"
  bootstrap: []  # Empty for first node
```

Note your bootstrap address after starting:
```
/ip4/<YOUR_PUBLIC_IP>/tcp/4001/p2p/<YOUR_PEER_ID>
```

### Subsequent Nodes

Configure existing nodes as bootstrap peers:

```yaml
mesh:
  gossip_key: "CAESQNAnotherPrivateKey..."
  listen_addrs:
    - "/ip4/0.0.0.0/tcp/4001"
  bootstrap:
    - "/ip4/203.0.113.10/tcp/4001/p2p/12D3KooWFirstNodePeerID..."
    - "/dns4/seed.mesh.example.com/tcp/4001/p2p/12D3KooWSecondNodePeerID..."
```

---

## Deploying a Backend Agent (meshagent)

Run this on every RPC/LCD node you want to add to the load balancer pool.

### 1. Create Configuration

Create `/etc/mesh/agent.yaml`:

```yaml
# Mesh network settings
mesh:
  gossip_key: "CAESQNYourAgentPrivateKey..."
  listen_addrs:
    - "/ip4/0.0.0.0/tcp/4001"
  bootstrap:
    - "/ip4/203.0.113.10/tcp/4001/p2p/12D3KooWBootstrapPeerID..."
  data_dir: "/var/lib/mesh/agent"
  anti_entropy_every: 10s

# Backend identification
backend:
  id: "backend-nyc-1"              # Unique identifier for this backend
  chain_id: "phoenix-1"            # Chain this backend serves
  host: "rpc-nyc-1.example.com"    # Hostname for this backend
  
  # Local endpoints to monitor
  rpc_endpoint: "http://localhost:26657"
  lcd_endpoint: "http://localhost:1317"
  prom_endpoint: "http://localhost:26660"
  
  # Cryptographic identity (for signature verification)
  identity_key: "CAESQNYourAgentPrivateKey..."
  owner_key: "terra1yourwalletaddress..."
  
  # Capabilities
  archival: false                  # Set true for archive nodes
  caps:
    rpc: true
    lcd: true
    ws: true
    grpc: false
  
  # Geographic location
  country: "US"                    # ISO 3166-1 alpha-2
  region: "us-east"
  ips:
    - "203.0.113.20"               # Public IP(s) of this backend

  # Optional metadata
  metadata:
    provider: "aws"
    datacenter: "us-east-1"

# Health probe settings
probe:
  interval: 10s                    # How often to check backend health
  timeout: 5s                      # Timeout for health checks
```

### 2. Create Data Directory

```bash
sudo mkdir -p /var/lib/mesh/agent
sudo chown $(whoami) /var/lib/mesh/agent
```

### 3. Start the Agent

```bash
./bin/meshagent -config /etc/mesh/agent.yaml
```

### 4. Verify Operation

The agent exposes a verification endpoint:

```bash
# Check agent is running and backend is healthy
curl http://localhost:18081/.mesh/verify
```

Expected output:
```json
{
  "backend_id": "backend-nyc-1",
  "chain_id": "phoenix-1",
  "healthy": true,
  "height": 12345678,
  "catching_up": false
}
```

### 5. Systemd Service (Production)

Create `/etc/systemd/system/meshagent.service`:

```ini
[Unit]
Description=Mesh Load Balancer Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=mesh
ExecStart=/usr/local/bin/meshagent -config /etc/mesh/agent.yaml
Restart=always
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable meshagent
sudo systemctl start meshagent
sudo journalctl -u meshagent -f
```

---

## Deploying an Edge Proxy (meshproxy)

Deploy edge proxies at locations close to your users. They receive client traffic and route to healthy backends.

### 1. Create Configuration

Create `/etc/mesh/edge.yaml`:

```yaml
# Mesh network settings
mesh:
  gossip_key: "CAESQNYourEdgePrivateKey..."
  listen_addrs:
    - "/ip4/0.0.0.0/tcp/4001"
  bootstrap:
    - "/ip4/203.0.113.10/tcp/4001/p2p/12D3KooWBootstrapPeerID..."
  data_dir: "/var/lib/mesh/edge"
  anti_entropy_every: 10s

# Edge proxy settings
edge:
  id: "edge-fra-1"                 # Unique identifier
  listen_http: ":8080"             # HTTP listener
  listen_https: ":443"             # HTTPS listener (optional)
  
  # Chains this edge serves
  chains:
    - "phoenix-1"
    - "columbus-5"
  
  # Geographic location
  country: "DE"
  region: "eu-west"
  
  # Public IPs to advertise (for DNS)
  advertise_ips:
    - "198.51.100.10"

# TLS settings (optional - for HTTPS)
tls:
  acme_email: "admin@example.com"
  domains:
    - "edge-fra.example.com"

# Rate limiting
rate_limit:
  per_ip_rps: 1000                 # Requests per second per client IP
  burst: 2000                      # Burst capacity

# Response cache
cache:
  enabled: true
  max_bytes: 536870912             # 512 MiB
  ttl_heighted: "infinite"         # Cache height-specific queries forever

# Backend selection tuning
selector:
  delta_height: 2                  # Max height lag for backend selection
  softmax_T: 0.7                   # Softmax temperature (lower = more deterministic)
```

### 2. Create Data Directory

```bash
sudo mkdir -p /var/lib/mesh/edge
sudo chown $(whoami) /var/lib/mesh/edge
```

### 3. Start the Edge Proxy

```bash
./bin/meshproxy -config /etc/mesh/edge.yaml
```

### 4. Verify Operation

```bash
# Health check
curl http://localhost:8080/health

# Proxy a request (requires backends to be registered)
curl -H "Host: rpc.phoenix-1.mesh.example.com" http://localhost:8080/status
```

### 5. Systemd Service (Production)

Create `/etc/systemd/system/meshproxy.service`:

```ini
[Unit]
Description=Mesh Load Balancer Edge Proxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=mesh
ExecStart=/usr/local/bin/meshproxy -config /etc/mesh/edge.yaml
Restart=always
RestartSec=5
LimitNOFILE=65535
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable meshproxy
sudo systemctl start meshproxy
```

---

## Deploying a DNS Server (meshdns)

DNS servers provide service discovery. Clients query `rpc.<chain>.<zone>` to discover edge proxies.

### 1. Create Configuration

Create `/etc/mesh/dns.yaml`:

```yaml
# Mesh network settings
mesh:
  gossip_key: "CAESQNYourDNSPrivateKey..."
  listen_addrs:
    - "/ip4/0.0.0.0/tcp/4001"
  bootstrap:
    - "/ip4/203.0.113.10/tcp/4001/p2p/12D3KooWBootstrapPeerID..."
  data_dir: "/var/lib/mesh/dns"
  anti_entropy_every: 2s           # Faster sync for DNS freshness

# DNS server settings
dns:
  listen: ":53"                    # Standard DNS port
  zone: "mesh.example.com"         # Your authoritative zone
  
  # Chains to serve
  chains:
    - "phoenix-1"
    - "columbus-5"
  
  # NS record labels
  ns_labels:
    - "ns1.mesh.example.com"
    - "ns2.mesh.example.com"
  
  # TTL settings
  ttl_ns_a: 30                     # TTL for NS/SOA records
  ttl_service: 20                  # TTL for service A records
  max_answers: 4                   # Max A records per response
  
  # EDNS Client Subnet (geo-aware responses)
  ecs: true
  
  # Geo-based DNS (returns geographically closest edges)
  geo_enabled: true
  geo_city_db: "/var/lib/mesh/GeoLite2-City.mmdb"
  geo_asn_db: "/var/lib/mesh/GeoLite2-ASN.mmdb"  # Optional
  geo_weight: 0.5                  # 0.0-1.0, higher = stronger geo preference
  geo_reload_sec: 86400            # Reload databases every 24 hours
  
  # Rate limiting
  rrl_qps: 10000
  rrl_burst: 20000
  rrl_window: 5m

# Backend selection tuning
selector:
  delta_height: 2
  softmax_T: 0.7
```

### 2. Set Up GeoIP Databases (Optional but Recommended)

For geo-based DNS to work, you need MaxMind GeoLite2 databases:

```bash
# Create a free MaxMind account at:
# https://www.maxmind.com/en/geolite2/signup

# Install geoipupdate
sudo apt-get install geoipupdate  # Debian/Ubuntu
# or
sudo yum install geoipupdate      # RHEL/CentOS

# Configure /etc/GeoIP.conf with your account ID and license key
# AccountID YOUR_ACCOUNT_ID
# LicenseKey YOUR_LICENSE_KEY
# EditionIDs GeoLite2-City GeoLite2-ASN

# Download databases
sudo geoipupdate

# Copy to mesh directory
sudo mkdir -p /var/lib/mesh
sudo cp /var/lib/GeoIP/GeoLite2-City.mmdb /var/lib/mesh/
sudo cp /var/lib/GeoIP/GeoLite2-ASN.mmdb /var/lib/mesh/
sudo chown mesh:mesh /var/lib/mesh/*.mmdb
```

> **Tip**: Set up a cron job to run `geoipupdate` weekly and the DNS server will automatically reload the databases.

### 3. Create Data Directory

```bash
sudo mkdir -p /var/lib/mesh/dns
sudo chown $(whoami) /var/lib/mesh/dns
```

### 3. Start the DNS Server

DNS requires port 53, which needs elevated privileges:

```bash
# Option 1: Run as root
sudo ./bin/meshdns -config /etc/mesh/dns.yaml

# Option 2: Use capabilities
sudo setcap 'cap_net_bind_service=+ep' ./bin/meshdns
./bin/meshdns -config /etc/mesh/dns.yaml
```

### 4. Verify Operation

```bash
# Test SOA record
dig @localhost mesh.example.com SOA

# Test NS records
dig @localhost mesh.example.com NS

# Test edge discovery
dig @localhost rpc.phoenix-1.mesh.example.com A
```

### 5. Configure Domain Delegation

At your domain registrar, delegate the zone to your DNS servers:

```
mesh.example.com.  NS  ns1.mesh.example.com.
mesh.example.com.  NS  ns2.mesh.example.com.
ns1.mesh.example.com.  A  198.51.100.53
ns2.mesh.example.com.  A  203.0.113.53
```

### 6. Systemd Service (Production)

Create `/etc/systemd/system/meshdns.service`:

```ini
[Unit]
Description=Mesh Load Balancer DNS Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=mesh
ExecStart=/usr/local/bin/meshdns -config /etc/mesh/dns.yaml
Restart=always
RestartSec=5
LimitNOFILE=65535
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
```

---

## Complete Example: 3-Node Setup

Here's a minimal production setup with one of each component:

### Node 1: Backend Agent (RPC Node)

**Server**: `rpc1.example.com` (203.0.113.20)

```yaml
# /etc/mesh/agent.yaml
mesh:
  gossip_key: "CAESQNAgentKey123..."
  listen_addrs: ["/ip4/0.0.0.0/tcp/4001"]
  bootstrap: []  # First node - no bootstrap
  data_dir: "/var/lib/mesh/agent"
  anti_entropy_every: 10s

backend:
  id: "backend-1"
  chain_id: "phoenix-1"
  host: "rpc1.example.com"
  rpc_endpoint: "http://localhost:26657"
  lcd_endpoint: "http://localhost:1317"
  identity_key: "CAESQNAgentKey123..."
  owner_key: "terra1owner..."
  caps: {rpc: true, lcd: true, ws: true}
  country: "US"
  region: "us-east"
  ips: ["203.0.113.20"]

probe:
  interval: 10s
  timeout: 5s
```

Get the peer ID after starting:
```bash
./bin/meshagent -config /etc/mesh/agent.yaml
# Look for: "peer_id":"12D3KooWAgentPeerID..."
```

Bootstrap address: `/ip4/203.0.113.20/tcp/4001/p2p/12D3KooWAgentPeerID...`

### Node 2: Edge Proxy

**Server**: `edge1.example.com` (198.51.100.10)

```yaml
# /etc/mesh/edge.yaml
mesh:
  gossip_key: "CAESQNEdgeKey456..."
  listen_addrs: ["/ip4/0.0.0.0/tcp/4001"]
  bootstrap:
    - "/ip4/203.0.113.20/tcp/4001/p2p/12D3KooWAgentPeerID..."
  data_dir: "/var/lib/mesh/edge"
  anti_entropy_every: 10s

edge:
  id: "edge-1"
  listen_http: ":8080"
  chains: ["phoenix-1"]
  country: "US"
  region: "us-west"
  advertise_ips: ["198.51.100.10"]

rate_limit:
  per_ip_rps: 1000
  burst: 2000

cache:
  enabled: true
  max_bytes: 536870912

selector:
  delta_height: 2
  softmax_T: 0.7
```

```bash
./bin/meshproxy -config /etc/mesh/edge.yaml
```

### Node 3: DNS Server

**Server**: `ns1.mesh.example.com` (198.51.100.53)

```yaml
# /etc/mesh/dns.yaml
mesh:
  gossip_key: "CAESQNDnsKey789..."
  listen_addrs: ["/ip4/0.0.0.0/tcp/4001"]
  bootstrap:
    - "/ip4/203.0.113.20/tcp/4001/p2p/12D3KooWAgentPeerID..."
  data_dir: "/var/lib/mesh/dns"
  anti_entropy_every: 2s

dns:
  listen: ":53"
  zone: "mesh.example.com"
  chains: ["phoenix-1"]
  ns_labels: ["ns1.mesh.example.com", "ns2.mesh.example.com"]
  ttl_ns_a: 30
  ttl_service: 20
  max_answers: 4
  ecs: true
  rrl_qps: 10000
  rrl_burst: 20000

selector:
  delta_height: 2
  softmax_T: 0.7
```

```bash
sudo ./bin/meshdns -config /etc/mesh/dns.yaml
```

### Verify the Setup

```bash
# 1. Check gossip connectivity (look for "peer connected" in logs)

# 2. Query DNS for edge proxy
dig @198.51.100.53 rpc.phoenix-1.mesh.example.com A
# Should return: 198.51.100.10

# 3. Query edge proxy for backend
curl -H "Host: rpc.phoenix-1.mesh.example.com" http://198.51.100.10:8080/status
# Should return blockchain status from backend

# 4. End-to-end test (after DNS delegation)
curl https://rpc.phoenix-1.mesh.example.com/status
```

---

## Troubleshooting

### Peers Not Connecting

1. Check firewall allows TCP 4001
2. Verify bootstrap address is correct (IP, port, peer ID)
3. Check logs for "peer connected" or connection errors

```bash
# Test connectivity
nc -zv <bootstrap_ip> 4001
```

### DNS Returns No Records

1. Verify edges are publishing NodeAdverts with `ns_serving: true`
2. Check anti-entropy sync in DNS logs ("merging anti-entropy response")
3. Ensure chain ID matches between edge and DNS config

### Backend Not Selected

1. Check agent is publishing metrics (look for "collected metrics" in logs)
2. Verify chain_id matches edge config
3. Check backend health status

```bash
curl http://<agent_ip>:18081/.mesh/verify
```

### High Latency

1. Enable caching on edge proxies
2. Deploy edges closer to users
3. Tune selector parameters

---

## Security Considerations

1. **Identity Keys**: Store securely, rotate periodically
2. **Network**: Use firewall to restrict gossip port (4001) to mesh peers only
3. **TLS**: Enable HTTPS on edge proxies for production
4. **Rate Limiting**: Configure appropriate limits for your traffic
5. **Monitoring**: Set up alerts for peer disconnections and health changes
