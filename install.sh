#!/bin/bash
#
# Mesh Load Balancer Installation Script
# 
# This script installs and configures mesh components interactively.
# Supported components: meshagent, meshproxy, meshdns
#
# Usage: ./install.sh
#

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

# Default values
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/mesh"
DATA_DIR="/var/lib/mesh"
MESH_USER="mesh"
GOSSIP_PORT="4001"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# State variables
COMPONENTS=()
BOOTSTRAP_PEERS=()
IS_FIRST_NODE="false"

#------------------------------------------------------------------------------
# Helper Functions
#------------------------------------------------------------------------------

print_banner() {
    echo -e "${CYAN}"
    echo "╔══════════════════════════════════════════════════════════════════╗"
    echo "║           Mesh Load Balancer Installation Script                 ║"
    echo "║                                                                  ║"
    echo "║  Components:                                                     ║"
    echo "║    • meshagent  - Backend agent (runs on RPC nodes)              ║"
    echo "║    • meshproxy  - Edge proxy (routes client requests)            ║"
    echo "║    • meshdns    - Authoritative DNS server                       ║"
    echo "╚══════════════════════════════════════════════════════════════════╝"
    echo -e "${NC}"
}

log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

prompt() {
    local message="$1"
    local default="$2"
    local result

    if [ -n "$default" ]; then
        read -p "$(echo -e "${CYAN}$message${NC} [$default]: ")" result
        echo "${result:-$default}"
    else
        read -p "$(echo -e "${CYAN}$message${NC}: ")" result
        echo "$result"
    fi
}

prompt_password() {
    local message="$1"
    local result
    read -s -p "$(echo -e "${CYAN}$message${NC}: ")" result
    echo ""
    echo "$result"
}

prompt_yes_no() {
    local message="$1"
    local default="$2"
    local result

    while true; do
        if [ "$default" = "y" ]; then
            read -p "$(echo -e "${CYAN}$message${NC} [Y/n]: ")" result
            result="${result:-y}"
        else
            read -p "$(echo -e "${CYAN}$message${NC} [y/N]: ")" result
            result="${result:-n}"
        fi

        case "${result,,}" in
            y|yes) echo "y"; return ;;
            n|no) echo "n"; return ;;
            *) echo "Please answer yes or no." >&2 ;;
        esac
    done
}

prompt_choice() {
    local message="$1"
    shift
    local options=("$@")
    local choice

    echo -e "${CYAN}$message${NC}"
    for i in "${!options[@]}"; do
        echo "  $((i+1))) ${options[$i]}"
    done

    while true; do
        read -p "$(echo -e "${CYAN}Enter choice [1-${#options[@]}]${NC}: ")" choice
        if [[ "$choice" =~ ^[0-9]+$ ]] && [ "$choice" -ge 1 ] && [ "$choice" -le "${#options[@]}" ]; then
            echo "$((choice-1))"
            return
        fi
        echo "Invalid choice. Please try again." >&2
    done
}

prompt_multi_choice() {
    local message="$1"
    shift
    local options=("$@")
    local selected=()
    local input
    
    # Extract short names from options (e.g., "meshagent (Backend agent...)" -> "meshagent")
    local short_names=()
    for opt in "${options[@]}"; do
        local short_name=$(echo "$opt" | awk '{print $1}')
        short_names+=("$short_name")
    done

    echo -e "${CYAN}$message${NC}" >&2
    for i in "${!options[@]}"; do
        echo "  $((i+1))) ${options[$i]}" >&2
    done
    echo "" >&2
    echo -e "${CYAN}Enter names or numbers, separated by commas (e.g., meshagent,meshproxy or 1,2,3)${NC}" >&2

    while true; do
        read -p "$(echo -e "${CYAN}Your selection${NC}: ")" input
        
        # Handle empty input
        if [ -z "$input" ]; then
            echo "Please enter at least one choice." >&2
            continue
        fi
        
        # Replace spaces with commas for space-separated input
        input=$(echo "$input" | tr ' ' ',')
        
        IFS=',' read -ra choices <<< "$input"
        selected=()
        local valid=true

        for choice in "${choices[@]}"; do
            choice=$(echo "$choice" | tr -d ' ')
            # Skip empty entries (e.g., from trailing comma)
            if [ -z "$choice" ]; then
                continue
            fi
            
            # Check if it's a number
            if [[ "$choice" =~ ^[0-9]+$ ]]; then
                if [ "$choice" -ge 1 ] && [ "$choice" -le "${#options[@]}" ]; then
                    selected+=("$((choice-1))")
                else
                    valid=false
                    echo "Invalid number: '$choice'. Please enter numbers between 1 and ${#options[@]}." >&2
                    break
                fi
            else
                # Check if it's a name
                local found=false
                for i in "${!short_names[@]}"; do
                    if [ "${short_names[$i]}" = "$choice" ]; then
                        selected+=("$i")
                        found=true
                        break
                    fi
                done
                if ! $found; then
                    valid=false
                    echo "Invalid choice: '$choice'. Valid names: ${short_names[*]}" >&2
                    break
                fi
            fi
        done

        if $valid && [ ${#selected[@]} -gt 0 ]; then
            echo "${selected[*]}"
            return
        fi
        if $valid; then
            echo "Please enter at least one choice." >&2
        fi
    done
}

detect_public_ip() {
    # Try multiple methods to detect public IP
    local ip=""
    
    # Try curl with various services
    for service in "ifconfig.me" "icanhazip.com" "api.ipify.org" "ipecho.net/plain"; do
        ip=$(curl -s --max-time 5 "$service" 2>/dev/null | grep -oE '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' || true)
        if [ -n "$ip" ]; then
            echo "$ip"
            return
        fi
    done

    # Try getting first non-local IP from hostname
    ip=$(hostname -I 2>/dev/null | awk '{print $1}' || true)
    if [ -n "$ip" ] && [[ ! "$ip" =~ ^(127\.|10\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.) ]]; then
        echo "$ip"
        return
    fi

    echo ""
}

detect_country() {
    # Try to detect country from IP geolocation
    local country=""
    country=$(curl -s --max-time 5 "http://ip-api.com/line/?fields=countryCode" 2>/dev/null || true)
    if [ -n "$country" ] && [[ "$country" =~ ^[A-Z]{2}$ ]]; then
        echo "$country"
        return
    fi
    echo "US"
}

check_root() {
    if [ "$EUID" -ne 0 ]; then
        log_error "This script must be run as root or with sudo"
        exit 1
    fi
}

check_dependencies() {
    local missing=()

    # Check for required tools
    for cmd in curl jq; do
        if ! command -v "$cmd" &>/dev/null; then
            missing+=("$cmd")
        fi
    done

    if [ ${#missing[@]} -gt 0 ]; then
        log_warning "Missing dependencies: ${missing[*]}"
        log_info "Installing missing dependencies..."
        
        if command -v apt-get &>/dev/null; then
            apt-get update && apt-get install -y "${missing[@]}"
        elif command -v yum &>/dev/null; then
            yum install -y "${missing[@]}"
        elif command -v dnf &>/dev/null; then
            dnf install -y "${missing[@]}"
        else
            log_error "Cannot install dependencies. Please install: ${missing[*]}"
            exit 1
        fi
    fi
}

#------------------------------------------------------------------------------
# Installation Functions
#------------------------------------------------------------------------------

install_binaries() {
    log_info "Installing mesh binaries..."

    # Check if binaries exist in script directory
    local bin_dir="$SCRIPT_DIR/bin"
    
    if [ ! -d "$bin_dir" ]; then
        log_warning "Binary directory not found at $bin_dir"
        
        if prompt_yes_no "Would you like to build from source?" "y" | grep -q "y"; then
            build_from_source
            bin_dir="$SCRIPT_DIR/bin"
        else
            log_error "Cannot proceed without binaries"
            exit 1
        fi
    fi

    # Install selected component binaries
    mkdir -p "$INSTALL_DIR"
    
    for idx in "${COMPONENTS[@]}"; do
        local component
        case $idx in
            0) component="meshagent" ;;
            1) component="meshproxy" ;;
            2) component="meshdns" ;;
        esac

        if [ -f "$bin_dir/$component" ]; then
            cp "$bin_dir/$component" "$INSTALL_DIR/"
            chmod +x "$INSTALL_DIR/$component"
            log_success "Installed $component to $INSTALL_DIR/"
        else
            log_error "Binary not found: $bin_dir/$component"
            exit 1
        fi
    done

    # Always install meshctl for key generation
    if [ -f "$bin_dir/meshctl" ]; then
        cp "$bin_dir/meshctl" "$INSTALL_DIR/"
        chmod +x "$INSTALL_DIR/meshctl"
        log_success "Installed meshctl to $INSTALL_DIR/"
    fi
}

build_from_source() {
    log_info "Building mesh from source..."

    # Check for Go
    if ! command -v go &>/dev/null; then
        log_error "Go is not installed. Please install Go 1.23+ first."
        log_info "Visit: https://golang.org/doc/install"
        exit 1
    fi

    # Check Go version
    local go_version
    go_version=$(go version | grep -oE 'go[0-9]+\.[0-9]+' | sed 's/go//')
    local major minor
    major=$(echo "$go_version" | cut -d. -f1)
    minor=$(echo "$go_version" | cut -d. -f2)

    if [ "$major" -lt 1 ] || ([ "$major" -eq 1 ] && [ "$minor" -lt 23 ]); then
        log_error "Go 1.23+ is required. Found: go$go_version"
        exit 1
    fi

    # Build
    cd "$SCRIPT_DIR"
    log_info "Running: go build -o bin/ ./cmd/..."
    go build -o bin/ ./cmd/...
    
    log_success "Build completed successfully"
}

#------------------------------------------------------------------------------
# GeoLite2 Database Download Functions
#------------------------------------------------------------------------------

# URLs for free GeoLite2 database mirrors (no license key required)
GEOLITE2_MIRROR_CITY="https://github.com/P3TERX/GeoLite.mmdb/raw/download/GeoLite2-City.mmdb"
GEOLITE2_MIRROR_ASN="https://github.com/P3TERX/GeoLite.mmdb/raw/download/GeoLite2-ASN.mmdb"

download_geolite2_free() {
    local city_db_path="$1"
    local asn_db_path="$2"
    
    log_info "Downloading GeoLite2 databases from public mirror..."
    log_info "Note: These are community-maintained mirrors of the free GeoLite2 databases."
    echo ""
    
    # Create target directory
    local city_dir
    city_dir=$(dirname "$city_db_path")
    mkdir -p "$city_dir"
    
    # Download GeoLite2-City
    log_info "Downloading GeoLite2-City.mmdb..."
    if curl -sS -L -o "$city_db_path" "$GEOLITE2_MIRROR_CITY"; then
        # Verify it's a valid mmdb file (starts with specific magic bytes)
        if file "$city_db_path" | grep -qE "(data|MaxMind)"; then
            chown "$MESH_USER:$MESH_USER" "$city_db_path" 2>/dev/null || true
            chmod 644 "$city_db_path"
            log_success "Installed GeoLite2-City.mmdb to $city_db_path"
        else
            log_error "Downloaded file is not a valid MMDB database"
            rm -f "$city_db_path"
            return 1
        fi
    else
        log_error "Failed to download GeoLite2-City.mmdb"
        return 1
    fi
    
    # Download GeoLite2-ASN if path provided
    if [ -n "$asn_db_path" ]; then
        local asn_dir
        asn_dir=$(dirname "$asn_db_path")
        mkdir -p "$asn_dir"
        
        log_info "Downloading GeoLite2-ASN.mmdb..."
        if curl -sS -L -o "$asn_db_path" "$GEOLITE2_MIRROR_ASN"; then
            if file "$asn_db_path" | grep -qE "(data|MaxMind)"; then
                chown "$MESH_USER:$MESH_USER" "$asn_db_path" 2>/dev/null || true
                chmod 644 "$asn_db_path"
                log_success "Installed GeoLite2-ASN.mmdb to $asn_db_path"
            else
                log_warning "Downloaded ASN file is not valid - skipping (optional)"
                rm -f "$asn_db_path"
            fi
        else
            log_warning "Failed to download GeoLite2-ASN.mmdb (optional)"
        fi
    fi
    
    return 0
}

download_geolite2_databases() {
    local license_key="$1"
    local city_db_path="$2"
    local asn_db_path="$3"
    
    local temp_dir
    temp_dir=$(mktemp -d)
    trap "rm -rf '$temp_dir'" EXIT
    
    local base_url="https://download.maxmind.com/app/geoip_download"
    
    log_info "Downloading GeoLite2 databases..."
    
    # Download GeoLite2-City
    log_info "Downloading GeoLite2-City.mmdb..."
    local city_url="${base_url}?edition_id=GeoLite2-City&license_key=${license_key}&suffix=tar.gz"
    
    if ! curl -sS -L -o "$temp_dir/GeoLite2-City.tar.gz" "$city_url"; then
        log_error "Failed to download GeoLite2-City database"
        log_info "Please check your license key and try again"
        return 1
    fi
    
    # Verify download is a valid archive (not an error page)
    if ! file "$temp_dir/GeoLite2-City.tar.gz" | grep -q "gzip"; then
        log_error "Downloaded file is not a valid archive"
        log_info "This usually means the license key is invalid"
        cat "$temp_dir/GeoLite2-City.tar.gz" 2>/dev/null | head -5
        return 1
    fi
    
    # Extract City database
    tar -xzf "$temp_dir/GeoLite2-City.tar.gz" -C "$temp_dir"
    local city_mmdb
    city_mmdb=$(find "$temp_dir" -name "GeoLite2-City.mmdb" -type f | head -1)
    
    if [ -z "$city_mmdb" ]; then
        log_error "Could not find GeoLite2-City.mmdb in archive"
        return 1
    fi
    
    # Create target directory if needed
    local city_dir
    city_dir=$(dirname "$city_db_path")
    mkdir -p "$city_dir"
    
    # Copy City database
    cp "$city_mmdb" "$city_db_path"
    chown "$MESH_USER:$MESH_USER" "$city_db_path"
    chmod 644 "$city_db_path"
    log_success "Installed GeoLite2-City.mmdb to $city_db_path"
    
    # Download GeoLite2-ASN if path provided
    if [ -n "$asn_db_path" ]; then
        log_info "Downloading GeoLite2-ASN.mmdb..."
        local asn_url="${base_url}?edition_id=GeoLite2-ASN&license_key=${license_key}&suffix=tar.gz"
        
        if ! curl -sS -L -o "$temp_dir/GeoLite2-ASN.tar.gz" "$asn_url"; then
            log_warning "Failed to download GeoLite2-ASN database (optional)"
        else
            # Verify download
            if file "$temp_dir/GeoLite2-ASN.tar.gz" | grep -q "gzip"; then
                tar -xzf "$temp_dir/GeoLite2-ASN.tar.gz" -C "$temp_dir"
                local asn_mmdb
                asn_mmdb=$(find "$temp_dir" -name "GeoLite2-ASN.mmdb" -type f | head -1)
                
                if [ -n "$asn_mmdb" ]; then
                    local asn_dir
                    asn_dir=$(dirname "$asn_db_path")
                    mkdir -p "$asn_dir"
                    cp "$asn_mmdb" "$asn_db_path"
                    chown "$MESH_USER:$MESH_USER" "$asn_db_path"
                    chmod 644 "$asn_db_path"
                    log_success "Installed GeoLite2-ASN.mmdb to $asn_db_path"
                fi
            else
                log_warning "GeoLite2-ASN download failed (optional)"
            fi
        fi
    fi
    
    return 0
}

setup_geoipupdate() {
    local account_id="$1"
    local license_key="$2"
    
    log_info "Setting up geoipupdate for automatic database updates..."
    
    # Install geoipupdate if not present
    if ! command -v geoipupdate &>/dev/null; then
        log_info "Installing geoipupdate..."
        if command -v apt-get &>/dev/null; then
            apt-get update && apt-get install -y geoipupdate
        elif command -v yum &>/dev/null; then
            yum install -y geoipupdate
        elif command -v dnf &>/dev/null; then
            dnf install -y geoipupdate
        else
            log_warning "Could not install geoipupdate automatically"
            log_info "Please install geoipupdate manually for automatic updates"
            return 1
        fi
    fi
    
    # Configure GeoIP.conf
    local geoip_conf="/etc/GeoIP.conf"
    
    cat > "$geoip_conf" << EOF
# GeoIP.conf file for geoipupdate
# Configured by mesh install.sh

AccountID $account_id
LicenseKey $license_key
EditionIDs GeoLite2-City GeoLite2-ASN
DatabaseDirectory /var/lib/GeoIP
EOF

    chmod 600 "$geoip_conf"
    log_success "Configured $geoip_conf"
    
    # Run initial update
    log_info "Running initial geoipupdate..."
    if geoipupdate -v; then
        log_success "GeoIP databases updated successfully"
    else
        log_warning "geoipupdate failed - you may need to run it manually"
    fi
    
    # Set up weekly cron job
    local cron_file="/etc/cron.weekly/geoipupdate"
    cat > "$cron_file" << 'EOF'
#!/bin/bash
# Update MaxMind GeoIP databases weekly
/usr/bin/geoipupdate -v >> /var/log/geoipupdate.log 2>&1
EOF
    chmod +x "$cron_file"
    log_success "Created weekly cron job: $cron_file"
    
    return 0
}

create_user() {
    log_info "Setting up mesh user..."

    if id "$MESH_USER" &>/dev/null; then
        log_info "User $MESH_USER already exists"
    else
        useradd --system --no-create-home --shell /bin/false "$MESH_USER"
        log_success "Created system user: $MESH_USER"
    fi
}

create_directories() {
    log_info "Creating directories..."

    mkdir -p "$CONFIG_DIR"
    mkdir -p "$DATA_DIR"
    
    for idx in "${COMPONENTS[@]}"; do
        local component
        case $idx in
            0) component="agent" ;;
            1) component="edge" ;;
            2) component="dns" ;;
        esac
        mkdir -p "$DATA_DIR/$component"
    done

    chown -R "$MESH_USER:$MESH_USER" "$DATA_DIR"
    chmod 750 "$DATA_DIR"
    chmod 750 "$CONFIG_DIR"
    
    log_success "Created directories"
}

generate_identity_key() {
    local component="$1"
    log_info "Generating identity key for $component..."

    local output
    output=$("$INSTALL_DIR/meshctl" genkeys 2>&1)
    
    local peer_id private_key
    peer_id=$(echo "$output" | grep "Public Key:" | awk '{print $3}')
    private_key=$(echo "$output" | grep "Private Key:" | awk '{print $3}')

    if [ -z "$peer_id" ] || [ -z "$private_key" ]; then
        log_error "Failed to generate identity key"
        exit 1
    fi

    echo "$peer_id|$private_key"
}

#------------------------------------------------------------------------------
# Configuration Collection Functions
#------------------------------------------------------------------------------

collect_common_config() {
    echo ""
    echo -e "${YELLOW}═══════════════════════════════════════════════════════════════════${NC}"
    echo -e "${YELLOW}                    Common Mesh Configuration                       ${NC}"
    echo -e "${YELLOW}═══════════════════════════════════════════════════════════════════${NC}"
    echo ""

    # First node check
    IS_FIRST_NODE=$(prompt_yes_no "Is this the first node in the mesh network?" "n")

    if [ "$IS_FIRST_NODE" = "n" ]; then
        echo ""
        log_info "Enter bootstrap peer addresses (LibP2P multiaddr format)"
        log_info "Format: /ip4/<IP>/tcp/<PORT>/p2p/<PEER_ID>"
        log_info "Example: /ip4/203.0.113.10/tcp/4001/p2p/12D3KooWExample..."
        echo ""

        while true; do
            local peer
            peer=$(prompt "Enter bootstrap peer (or 'done' to finish)" "")
            
            if [ "$peer" = "done" ] || [ -z "$peer" ]; then
                if [ ${#BOOTSTRAP_PEERS[@]} -eq 0 ]; then
                    log_warning "No bootstrap peers entered. Node will wait for incoming connections."
                fi
                break
            fi

            BOOTSTRAP_PEERS+=("$peer")
            log_success "Added bootstrap peer: $peer"
        done
    else
        log_info "This node will be a bootstrap seed. Other nodes should connect to this node."
    fi

    # Gossip port
    GOSSIP_PORT=$(prompt "Gossip port (LibP2P)" "$GOSSIP_PORT")
}

collect_agent_config() {
    echo ""
    echo -e "${YELLOW}═══════════════════════════════════════════════════════════════════${NC}"
    echo -e "${YELLOW}                   Backend Agent Configuration                      ${NC}"
    echo -e "${YELLOW}═══════════════════════════════════════════════════════════════════${NC}"
    echo ""

    # Generate identity
    local identity_result
    identity_result=$(generate_identity_key "agent")
    AGENT_PEER_ID=$(echo "$identity_result" | cut -d'|' -f1)
    AGENT_PRIVATE_KEY=$(echo "$identity_result" | cut -d'|' -f2)
    
    log_success "Generated agent identity:"
    log_info "  Peer ID: $AGENT_PEER_ID"
    echo ""

    # Backend settings
    AGENT_ID=$(prompt "Backend ID (unique identifier)" "backend-$(hostname -s)")
    AGENT_CHAIN_ID=$(prompt "Chain ID" "phoenix-1")
    AGENT_HOST=$(prompt "Backend hostname" "$(hostname -f)")
    
    echo ""
    log_info "Local endpoint configuration:"
    AGENT_RPC_ENDPOINT=$(prompt "RPC endpoint" "http://localhost:26657")
    AGENT_LCD_ENDPOINT=$(prompt "LCD/REST endpoint" "http://localhost:1317")
    AGENT_PROM_ENDPOINT=$(prompt "Prometheus endpoint (optional)" "http://localhost:26660")

    echo ""
    log_info "Backend identity:"
    AGENT_OWNER_KEY=$(prompt "Owner wallet address" "terra1...")

    echo ""
    log_info "Backend capabilities:"
    AGENT_CAP_RPC=$(prompt_yes_no "Serves RPC?" "y")
    AGENT_CAP_LCD=$(prompt_yes_no "Serves LCD/REST?" "y")
    AGENT_CAP_WS=$(prompt_yes_no "Serves WebSocket?" "y")
    AGENT_CAP_GRPC=$(prompt_yes_no "Serves gRPC?" "n")
    AGENT_ARCHIVAL=$(prompt_yes_no "Is this an archive node?" "n")

    echo ""
    log_info "Geographic location:"
    local detected_country
    detected_country=$(detect_country)
    AGENT_COUNTRY=$(prompt "Country code (ISO 3166-1 alpha-2)" "$detected_country")
    AGENT_REGION=$(prompt "Region" "us-east")

    echo ""
    log_info "Network configuration:"
    local detected_ip
    detected_ip=$(detect_public_ip)
    AGENT_PUBLIC_IP=$(prompt "Public IP address" "$detected_ip")

    echo ""
    log_info "Probe settings:"
    AGENT_PROBE_INTERVAL=$(prompt "Health check interval" "10s")
    AGENT_PROBE_TIMEOUT=$(prompt "Health check timeout" "5s")

    # Optional metadata
    echo ""
    if prompt_yes_no "Add optional metadata?" "n" | grep -q "y"; then
        AGENT_PROVIDER=$(prompt "Provider (aws, gcp, azure, bare-metal)" "bare-metal")
        AGENT_DATACENTER=$(prompt "Datacenter" "dc1")
    else
        AGENT_PROVIDER=""
        AGENT_DATACENTER=""
    fi
}

collect_edge_config() {
    echo ""
    echo -e "${YELLOW}═══════════════════════════════════════════════════════════════════${NC}"
    echo -e "${YELLOW}                    Edge Proxy Configuration                        ${NC}"
    echo -e "${YELLOW}═══════════════════════════════════════════════════════════════════${NC}"
    echo ""

    # Generate identity
    local identity_result
    identity_result=$(generate_identity_key "edge")
    EDGE_PEER_ID=$(echo "$identity_result" | cut -d'|' -f1)
    EDGE_PRIVATE_KEY=$(echo "$identity_result" | cut -d'|' -f2)
    
    log_success "Generated edge identity:"
    log_info "  Peer ID: $EDGE_PEER_ID"
    echo ""

    # Edge settings
    EDGE_ID=$(prompt "Edge ID (unique identifier)" "edge-$(hostname -s)")
    EDGE_LISTEN_HTTP=$(prompt "HTTP listen address" ":8080")

    if prompt_yes_no "Enable HTTPS?" "n" | grep -q "y"; then
        EDGE_ENABLE_HTTPS="true"
        EDGE_LISTEN_HTTPS=$(prompt "HTTPS listen address" ":443")
        
        if prompt_yes_no "Use ACME (Let's Encrypt) for TLS?" "y" | grep -q "y"; then
            EDGE_USE_ACME="true"
            EDGE_ACME_EMAIL=$(prompt "ACME email address" "admin@example.com")
            EDGE_ACME_DOMAIN=$(prompt "Domain name" "edge.example.com")
        else
            EDGE_USE_ACME="false"
            EDGE_TLS_CERT=$(prompt "TLS certificate path" "/etc/mesh/tls/cert.pem")
            EDGE_TLS_KEY=$(prompt "TLS private key path" "/etc/mesh/tls/key.pem")
        fi
    else
        EDGE_ENABLE_HTTPS="false"
    fi

    echo ""
    log_info "Chain configuration:"
    EDGE_CHAINS=()
    while true; do
        local chain
        chain=$(prompt "Enter chain ID to serve (or 'done' to finish)" "phoenix-1")
        
        if [ "$chain" = "done" ]; then
            break
        fi

        EDGE_CHAINS+=("$chain")
        log_success "Added chain: $chain"
        
        if ! prompt_yes_no "Add another chain?" "n" | grep -q "y"; then
            break
        fi
    done

    echo ""
    log_info "Geographic location:"
    local detected_country
    detected_country=$(detect_country)
    EDGE_COUNTRY=$(prompt "Country code (ISO 3166-1 alpha-2)" "$detected_country")
    EDGE_REGION=$(prompt "Region" "us-east")

    echo ""
    log_info "Network configuration:"
    local detected_ip
    detected_ip=$(detect_public_ip)
    EDGE_PUBLIC_IP=$(prompt "Public IP address (for DNS advertisement)" "$detected_ip")

    echo ""
    log_info "Rate limiting:"
    EDGE_RATE_LIMIT_RPS=$(prompt "Requests per second per IP" "1000")
    EDGE_RATE_LIMIT_BURST=$(prompt "Burst capacity" "2000")

    echo ""
    log_info "Cache settings:"
    EDGE_CACHE_ENABLED=$(prompt_yes_no "Enable response cache?" "y")
    if [ "$EDGE_CACHE_ENABLED" = "y" ]; then
        EDGE_CACHE_SIZE=$(prompt "Cache size in MiB" "512")
    fi

    echo ""
    log_info "Selector tuning:"
    EDGE_DELTA_HEIGHT=$(prompt "Max height lag for backend selection" "2")
    EDGE_SOFTMAX_T=$(prompt "Softmax temperature (0.1-2.0, lower = more deterministic)" "0.7")
}

collect_dns_config() {
    echo ""
    echo -e "${YELLOW}═══════════════════════════════════════════════════════════════════${NC}"
    echo -e "${YELLOW}                    DNS Server Configuration                        ${NC}"
    echo -e "${YELLOW}═══════════════════════════════════════════════════════════════════${NC}"
    echo ""

    # Generate identity
    local identity_result
    identity_result=$(generate_identity_key "dns")
    DNS_PEER_ID=$(echo "$identity_result" | cut -d'|' -f1)
    DNS_PRIVATE_KEY=$(echo "$identity_result" | cut -d'|' -f2)
    
    log_success "Generated DNS identity:"
    log_info "  Peer ID: $DNS_PEER_ID"
    echo ""

    # DNS settings
    DNS_LISTEN=$(prompt "DNS listen address" ":53")
    DNS_ZONE=$(prompt "Authoritative zone" "mesh.example.com")

    echo ""
    log_info "Chain configuration:"
    DNS_CHAINS=()
    while true; do
        local chain
        chain=$(prompt "Enter chain ID to serve (or 'done' to finish)" "phoenix-1")
        
        if [ "$chain" = "done" ]; then
            break
        fi

        DNS_CHAINS+=("$chain")
        log_success "Added chain: $chain"
        
        if ! prompt_yes_no "Add another chain?" "n" | grep -q "y"; then
            break
        fi
    done

    echo ""
    log_info "NS record configuration:"
    DNS_NS_LABELS=()
    log_info "Enter NS labels for your zone (e.g., ns1.mesh.example.com)"
    while true; do
        local ns
        ns=$(prompt "Enter NS label (or 'done' to finish)" "ns1.$DNS_ZONE")
        
        if [ "$ns" = "done" ]; then
            break
        fi

        DNS_NS_LABELS+=("$ns")
        log_success "Added NS: $ns"
        
        if ! prompt_yes_no "Add another NS?" "y" | grep -q "y"; then
            break
        fi
    done

    echo ""
    log_info "TTL settings:"
    DNS_TTL_NS_A=$(prompt "TTL for NS/SOA records (seconds)" "30")
    DNS_TTL_SERVICE=$(prompt "TTL for service A records (seconds)" "20")
    DNS_MAX_ANSWERS=$(prompt "Maximum A records per response" "4")

    echo ""
    log_info "EDNS Client Subnet (geo-aware responses):"
    DNS_ECS_ENABLED=$(prompt_yes_no "Enable ECS?" "y")

    echo ""
    log_info "Geo-based DNS (returns geographically closest edges to clients):"
    DNS_GEO_ENABLED=$(prompt_yes_no "Enable geo-based DNS?" "y")
    
    if [ "$DNS_GEO_ENABLED" = "y" ]; then
        echo ""
        log_info "MaxMind GeoIP2/GeoLite2 database configuration:"
        log_info "GeoLite2 databases are free but require a MaxMind account."
        log_info "Sign up at: https://www.maxmind.com/en/geolite2/signup"
        echo ""
        
        DNS_GEO_CITY_DB=$(prompt "Path to GeoLite2-City.mmdb" "/var/lib/mesh/GeoLite2-City.mmdb")
        
        if prompt_yes_no "Configure ASN database (optional, improves accuracy)?" "y" | grep -q "y"; then
            DNS_GEO_ASN_DB=$(prompt "Path to GeoLite2-ASN.mmdb" "/var/lib/mesh/GeoLite2-ASN.mmdb")
        else
            DNS_GEO_ASN_DB=""
        fi
        
        DNS_GEO_WEIGHT=$(prompt "Geo weight (0.0-1.0, higher = stronger geo preference)" "0.5")
        
        if prompt_yes_no "Enable automatic database reload?" "y" | grep -q "y"; then
            DNS_GEO_RELOAD_SEC=$(prompt "Reload interval (seconds, 86400 = 24 hours)" "86400")
        else
            DNS_GEO_RELOAD_SEC="0"
        fi
        
        # Check if databases exist and offer to download
        if [ ! -f "$DNS_GEO_CITY_DB" ]; then
            echo ""
            log_warning "GeoLite2-City database not found at: $DNS_GEO_CITY_DB"
            echo ""
            
            if prompt_yes_no "Would you like to download GeoLite2 databases now?" "y" | grep -q "y"; then
                echo ""
                
                # Choose download method
                local download_method
                download_method=$(prompt_choice "Choose download method:" \
                    "Free mirror (no account required, community-maintained)" \
                    "MaxMind direct (requires free license key)" \
                    "Setup geoipupdate (automatic updates, requires license key)")
                
                case "$download_method" in
                    0)
                        # Free mirror download
                        echo ""
                        if download_geolite2_free "$DNS_GEO_CITY_DB" "$DNS_GEO_ASN_DB"; then
                            log_success "GeoLite2 databases installed successfully"
                        else
                            log_warning "Mirror download failed - trying alternative..."
                            # Try alternative mirror
                            GEOLITE2_MIRROR_CITY="https://git.io/GeoLite2-City.mmdb"
                            GEOLITE2_MIRROR_ASN="https://git.io/GeoLite2-ASN.mmdb"
                            if download_geolite2_free "$DNS_GEO_CITY_DB" "$DNS_GEO_ASN_DB"; then
                                log_success "GeoLite2 databases installed from alternative mirror"
                            else
                                log_warning "Database download failed - you can download manually later"
                            fi
                        fi
                        ;;
                    1)
                        # MaxMind direct download
                        echo ""
                        log_info "Get your free license key from: https://www.maxmind.com/en/accounts/current/license-key"
                        MAXMIND_LICENSE_KEY=$(prompt "Enter your MaxMind license key" "")
                        
                        if [ -n "$MAXMIND_LICENSE_KEY" ]; then
                            if download_geolite2_databases "$MAXMIND_LICENSE_KEY" "$DNS_GEO_CITY_DB" "$DNS_GEO_ASN_DB"; then
                                log_success "GeoLite2 databases installed successfully"
                            else
                                log_warning "MaxMind download failed - trying free mirror..."
                                download_geolite2_free "$DNS_GEO_CITY_DB" "$DNS_GEO_ASN_DB"
                            fi
                        else
                            log_warning "No license key provided - using free mirror instead..."
                            download_geolite2_free "$DNS_GEO_CITY_DB" "$DNS_GEO_ASN_DB"
                        fi
                        ;;
                    2)
                        # Setup geoipupdate
                        echo ""
                        log_info "Get your credentials from: https://www.maxmind.com/en/accounts/current/license-key"
                        MAXMIND_ACCOUNT_ID=$(prompt "Enter your MaxMind Account ID" "")
                        MAXMIND_LICENSE_KEY=$(prompt "Enter your MaxMind license key" "")
                        
                        if [ -n "$MAXMIND_ACCOUNT_ID" ] && [ -n "$MAXMIND_LICENSE_KEY" ]; then
                            if setup_geoipupdate "$MAXMIND_ACCOUNT_ID" "$MAXMIND_LICENSE_KEY"; then
                                # Copy databases to mesh directory
                                if [ -f "/var/lib/GeoIP/GeoLite2-City.mmdb" ]; then
                                    local city_dir
                                    city_dir=$(dirname "$DNS_GEO_CITY_DB")
                                    mkdir -p "$city_dir"
                                    cp "/var/lib/GeoIP/GeoLite2-City.mmdb" "$DNS_GEO_CITY_DB"
                                    chown "$MESH_USER:$MESH_USER" "$DNS_GEO_CITY_DB" 2>/dev/null || true
                                    log_success "Copied GeoLite2-City.mmdb to $DNS_GEO_CITY_DB"
                                fi
                                if [ -n "$DNS_GEO_ASN_DB" ] && [ -f "/var/lib/GeoIP/GeoLite2-ASN.mmdb" ]; then
                                    local asn_dir
                                    asn_dir=$(dirname "$DNS_GEO_ASN_DB")
                                    mkdir -p "$asn_dir"
                                    cp "/var/lib/GeoIP/GeoLite2-ASN.mmdb" "$DNS_GEO_ASN_DB"
                                    chown "$MESH_USER:$MESH_USER" "$DNS_GEO_ASN_DB" 2>/dev/null || true
                                    log_success "Copied GeoLite2-ASN.mmdb to $DNS_GEO_ASN_DB"
                                fi
                            else
                                log_warning "geoipupdate setup failed - using free mirror..."
                                download_geolite2_free "$DNS_GEO_CITY_DB" "$DNS_GEO_ASN_DB"
                            fi
                        else
                            log_warning "Credentials not provided - using free mirror instead..."
                            download_geolite2_free "$DNS_GEO_CITY_DB" "$DNS_GEO_ASN_DB"
                        fi
                        ;;
                esac
            else
                echo ""
                log_info "You can download the databases later. Options:"
                echo ""
                echo "  Option 1: Free mirror (no account required)"
                echo "    curl -L -o $DNS_GEO_CITY_DB \\"
                echo "      'https://github.com/P3TERX/GeoLite.mmdb/raw/download/GeoLite2-City.mmdb'"
                if [ -n "$DNS_GEO_ASN_DB" ]; then
                    echo "    curl -L -o $DNS_GEO_ASN_DB \\"
                    echo "      'https://github.com/P3TERX/GeoLite.mmdb/raw/download/GeoLite2-ASN.mmdb'"
                fi
                echo ""
                echo "  Option 2: MaxMind direct (requires free account)"
                echo "    1. Sign up at: https://www.maxmind.com/en/geolite2/signup"
                echo "    2. Download from: https://www.maxmind.com/en/accounts/current/geoip/downloads"
                echo ""
            fi
        else
            log_success "GeoLite2-City database found at: $DNS_GEO_CITY_DB"
            if [ -n "$DNS_GEO_ASN_DB" ]; then
                if [ -f "$DNS_GEO_ASN_DB" ]; then
                    log_success "GeoLite2-ASN database found at: $DNS_GEO_ASN_DB"
                else
                    log_warning "GeoLite2-ASN database not found at: $DNS_GEO_ASN_DB"
                    if prompt_yes_no "Download GeoLite2-ASN from free mirror?" "y" | grep -q "y"; then
                        log_info "Downloading GeoLite2-ASN.mmdb..."
                        local asn_dir
                        asn_dir=$(dirname "$DNS_GEO_ASN_DB")
                        mkdir -p "$asn_dir"
                        if curl -sS -L -o "$DNS_GEO_ASN_DB" "$GEOLITE2_MIRROR_ASN"; then
                            chown "$MESH_USER:$MESH_USER" "$DNS_GEO_ASN_DB" 2>/dev/null || true
                            chmod 644 "$DNS_GEO_ASN_DB"
                            log_success "Installed GeoLite2-ASN.mmdb"
                        else
                            log_warning "Download failed"
                        fi
                    fi
                fi
            fi
        fi
    else
        DNS_GEO_CITY_DB=""
        DNS_GEO_ASN_DB=""
        DNS_GEO_WEIGHT="0"
        DNS_GEO_RELOAD_SEC="0"
    fi

    echo ""
    log_info "Rate limiting (Response Rate Limiting):"
    DNS_RRL_QPS=$(prompt "Queries per second limit" "10000")
    DNS_RRL_BURST=$(prompt "Burst capacity" "20000")
    DNS_RRL_WINDOW=$(prompt "Rate limit window" "5m")

    echo ""
    log_info "Selector tuning:"
    DNS_DELTA_HEIGHT=$(prompt "Max height lag for backend selection" "2")
    DNS_SOFTMAX_T=$(prompt "Softmax temperature (0.1-2.0, lower = more deterministic)" "0.7")
}

#------------------------------------------------------------------------------
# Configuration Generation Functions
#------------------------------------------------------------------------------

generate_bootstrap_yaml() {
    local yaml=""
    if [ ${#BOOTSTRAP_PEERS[@]} -gt 0 ]; then
        yaml="  bootstrap:"
        for peer in "${BOOTSTRAP_PEERS[@]}"; do
            yaml+=$'\n'"    - \"$peer\""
        done
    else
        yaml="  bootstrap: []"
    fi
    echo "$yaml"
}

generate_agent_config() {
    local config_file="$CONFIG_DIR/agent.yaml"
    local bootstrap_yaml
    bootstrap_yaml=$(generate_bootstrap_yaml)

    local metadata_yaml=""
    if [ -n "$AGENT_PROVIDER" ]; then
        metadata_yaml="  metadata:
    provider: \"$AGENT_PROVIDER\"
    datacenter: \"$AGENT_DATACENTER\""
    fi

    cat > "$config_file" << EOF
# Mesh Backend Agent Configuration
# Generated by install.sh on $(date)

# Mesh/LibP2P settings
mesh:
  gossip_key: "$AGENT_PRIVATE_KEY"
  listen_addrs:
    - "/ip4/0.0.0.0/tcp/$GOSSIP_PORT"
$bootstrap_yaml
  data_dir: "$DATA_DIR/agent"
  anti_entropy_every: 10s

# Backend settings
backend:
  id: "$AGENT_ID"
  chain_id: "$AGENT_CHAIN_ID"
  host: "$AGENT_HOST"
  rpc_endpoint: "$AGENT_RPC_ENDPOINT"
  lcd_endpoint: "$AGENT_LCD_ENDPOINT"
  prom_endpoint: "$AGENT_PROM_ENDPOINT"
  identity_key: "$AGENT_PRIVATE_KEY"
  owner_key: "$AGENT_OWNER_KEY"
  archival: $( [ "$AGENT_ARCHIVAL" = "y" ] && echo "true" || echo "false" )
  caps:
    rpc: $( [ "$AGENT_CAP_RPC" = "y" ] && echo "true" || echo "false" )
    lcd: $( [ "$AGENT_CAP_LCD" = "y" ] && echo "true" || echo "false" )
    ws: $( [ "$AGENT_CAP_WS" = "y" ] && echo "true" || echo "false" )
    grpc: $( [ "$AGENT_CAP_GRPC" = "y" ] && echo "true" || echo "false" )
  country: "$AGENT_COUNTRY"
  region: "$AGENT_REGION"
  ips:
    - "$AGENT_PUBLIC_IP"
$metadata_yaml

# Probe settings
probe:
  interval: $AGENT_PROBE_INTERVAL
  timeout: $AGENT_PROBE_TIMEOUT
EOF

    chown "$MESH_USER:$MESH_USER" "$config_file"
    chmod 640 "$config_file"
    log_success "Created agent configuration: $config_file"
}

generate_edge_config() {
    local config_file="$CONFIG_DIR/edge.yaml"
    local bootstrap_yaml
    bootstrap_yaml=$(generate_bootstrap_yaml)

    # Build chains array
    local chains_yaml="  chains:"
    for chain in "${EDGE_CHAINS[@]}"; do
        chains_yaml+=$'\n'"    - \"$chain\""
    done

    # Build TLS section
    local tls_yaml=""
    if [ "$EDGE_ENABLE_HTTPS" = "true" ]; then
        if [ "$EDGE_USE_ACME" = "true" ]; then
            tls_yaml="# TLS settings (ACME)
tls:
  acme_email: \"$EDGE_ACME_EMAIL\"
  domains:
    - \"$EDGE_ACME_DOMAIN\""
        else
            tls_yaml="# TLS settings (manual)
tls:
  cert_file: \"$EDGE_TLS_CERT\"
  key_file: \"$EDGE_TLS_KEY\""
        fi
    fi

    # Build cache section
    local cache_yaml=""
    if [ "$EDGE_CACHE_ENABLED" = "y" ]; then
        local cache_bytes=$((EDGE_CACHE_SIZE * 1024 * 1024))
        cache_yaml="# Cache settings
cache:
  enabled: true
  max_bytes: $cache_bytes
  ttl_heighted: \"infinite\""
    else
        cache_yaml="# Cache settings
cache:
  enabled: false"
    fi

    cat > "$config_file" << EOF
# Mesh Edge Proxy Configuration
# Generated by install.sh on $(date)

# Mesh/LibP2P settings
mesh:
  gossip_key: "$EDGE_PRIVATE_KEY"
  listen_addrs:
    - "/ip4/0.0.0.0/tcp/$GOSSIP_PORT"
$bootstrap_yaml
  data_dir: "$DATA_DIR/edge"
  anti_entropy_every: 10s

# Edge proxy settings
edge:
  id: "$EDGE_ID"
  listen_http: "$EDGE_LISTEN_HTTP"
$( [ "$EDGE_ENABLE_HTTPS" = "true" ] && echo "  listen_https: \"$EDGE_LISTEN_HTTPS\"" )
$chains_yaml
  country: "$EDGE_COUNTRY"
  region: "$EDGE_REGION"
  advertise_ips:
    - "$EDGE_PUBLIC_IP"

$tls_yaml

# Rate limiting
rate_limit:
  per_ip_rps: $EDGE_RATE_LIMIT_RPS
  burst: $EDGE_RATE_LIMIT_BURST

$cache_yaml

# Selector tuning
selector:
  delta_height: $EDGE_DELTA_HEIGHT
  softmax_T: $EDGE_SOFTMAX_T
EOF

    chown "$MESH_USER:$MESH_USER" "$config_file"
    chmod 640 "$config_file"
    log_success "Created edge configuration: $config_file"
}

generate_dns_config() {
    local config_file="$CONFIG_DIR/dns.yaml"
    local bootstrap_yaml
    bootstrap_yaml=$(generate_bootstrap_yaml)

    # Build chains array
    local chains_yaml="  chains:"
    for chain in "${DNS_CHAINS[@]}"; do
        chains_yaml+=$'\n'"    - \"$chain\""
    done

    # Build NS labels array
    local ns_yaml="  ns_labels:"
    for ns in "${DNS_NS_LABELS[@]}"; do
        ns_yaml+=$'\n'"    - \"$ns\""
    done

    # Build geo settings
    local geo_yaml=""
    if [ "$DNS_GEO_ENABLED" = "y" ]; then
        geo_yaml="  # Geo-based DNS settings
  geo_enabled: true
  geo_city_db: \"$DNS_GEO_CITY_DB\""
        if [ -n "$DNS_GEO_ASN_DB" ]; then
            geo_yaml+=$'\n'"  geo_asn_db: \"$DNS_GEO_ASN_DB\""
        fi
        geo_yaml+=$'\n'"  geo_weight: $DNS_GEO_WEIGHT"
        geo_yaml+=$'\n'"  geo_reload_sec: $DNS_GEO_RELOAD_SEC"
    else
        geo_yaml="  # Geo-based DNS (disabled)
  geo_enabled: false"
    fi

    cat > "$config_file" << EOF
# Mesh DNS Server Configuration
# Generated by install.sh on $(date)

# Mesh/LibP2P settings
mesh:
  gossip_key: "$DNS_PRIVATE_KEY"
  listen_addrs:
    - "/ip4/0.0.0.0/tcp/$GOSSIP_PORT"
$bootstrap_yaml
  data_dir: "$DATA_DIR/dns"
  anti_entropy_every: 2s

# DNS server settings
dns:
  listen: "$DNS_LISTEN"
  zone: "$DNS_ZONE"
$chains_yaml
$ns_yaml
  ttl_ns_a: $DNS_TTL_NS_A
  ttl_service: $DNS_TTL_SERVICE
  max_answers: $DNS_MAX_ANSWERS
  ecs: $( [ "$DNS_ECS_ENABLED" = "y" ] && echo "true" || echo "false" )
  dnssec: false
  # Response Rate Limiting
  rrl_qps: $DNS_RRL_QPS
  rrl_burst: $DNS_RRL_BURST
  rrl_window: $DNS_RRL_WINDOW
$geo_yaml

# Selector tuning
selector:
  delta_height: $DNS_DELTA_HEIGHT
  softmax_T: $DNS_SOFTMAX_T
EOF

    chown "$MESH_USER:$MESH_USER" "$config_file"
    chmod 640 "$config_file"
    log_success "Created DNS configuration: $config_file"
}

#------------------------------------------------------------------------------
# Systemd Service Functions
#------------------------------------------------------------------------------

create_agent_service() {
    local service_file="/etc/systemd/system/meshagent.service"
    
    cat > "$service_file" << EOF
[Unit]
Description=Mesh Load Balancer Agent
Documentation=https://github.com/lunc/mesh
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$MESH_USER
Group=$MESH_USER
ExecStart=$INSTALL_DIR/meshagent -config $CONFIG_DIR/agent.yaml
Restart=always
RestartSec=5
LimitNOFILE=65535

# Security hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$DATA_DIR/agent
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

    log_success "Created systemd service: $service_file"
}

create_edge_service() {
    local service_file="/etc/systemd/system/meshproxy.service"
    local capabilities=""
    
    if [ "$EDGE_ENABLE_HTTPS" = "true" ] && [ "$EDGE_LISTEN_HTTPS" = ":443" ]; then
        capabilities="AmbientCapabilities=CAP_NET_BIND_SERVICE"
    fi
    
    cat > "$service_file" << EOF
[Unit]
Description=Mesh Load Balancer Edge Proxy
Documentation=https://github.com/lunc/mesh
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$MESH_USER
Group=$MESH_USER
ExecStart=$INSTALL_DIR/meshproxy -config $CONFIG_DIR/edge.yaml
Restart=always
RestartSec=5
LimitNOFILE=65535
$capabilities

# Security hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$DATA_DIR/edge
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

    log_success "Created systemd service: $service_file"
}

create_dns_service() {
    local service_file="/etc/systemd/system/meshdns.service"
    
    cat > "$service_file" << EOF
[Unit]
Description=Mesh Load Balancer DNS Server
Documentation=https://github.com/lunc/mesh
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$MESH_USER
Group=$MESH_USER
ExecStart=$INSTALL_DIR/meshdns -config $CONFIG_DIR/dns.yaml
Restart=always
RestartSec=5
LimitNOFILE=65535
AmbientCapabilities=CAP_NET_BIND_SERVICE

# Security hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$DATA_DIR/dns
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

    log_success "Created systemd service: $service_file"
}

enable_services() {
    log_info "Enabling systemd services..."
    
    systemctl daemon-reload

    for idx in "${COMPONENTS[@]}"; do
        local service
        case $idx in
            0) service="meshagent" ;;
            1) service="meshproxy" ;;
            2) service="meshdns" ;;
        esac

        systemctl enable "$service"
        log_success "Enabled $service service"
    done
}

#------------------------------------------------------------------------------
# Firewall Configuration
#------------------------------------------------------------------------------

configure_firewall() {
    echo ""
    if ! prompt_yes_no "Configure firewall rules?" "y" | grep -q "y"; then
        return
    fi

    log_info "Configuring firewall..."

    # Detect firewall
    if command -v ufw &>/dev/null && ufw status | grep -q "active"; then
        configure_ufw
    elif command -v firewall-cmd &>/dev/null && systemctl is-active firewalld &>/dev/null; then
        configure_firewalld
    elif command -v iptables &>/dev/null; then
        configure_iptables
    else
        log_warning "No supported firewall detected. Please configure manually."
        print_firewall_requirements
    fi
}

configure_ufw() {
    log_info "Configuring UFW..."
    
    # Gossip port
    ufw allow "$GOSSIP_PORT/tcp" comment "Mesh gossip"
    
    for idx in "${COMPONENTS[@]}"; do
        case $idx in
            0)  # Agent
                ufw allow 18081/tcp comment "Mesh agent verification"
                ;;
            1)  # Edge
                local http_port="${EDGE_LISTEN_HTTP#:}"
                ufw allow "$http_port/tcp" comment "Mesh edge HTTP"
                if [ "$EDGE_ENABLE_HTTPS" = "true" ]; then
                    local https_port="${EDGE_LISTEN_HTTPS#:}"
                    ufw allow "$https_port/tcp" comment "Mesh edge HTTPS"
                fi
                ;;
            2)  # DNS
                local dns_port="${DNS_LISTEN#:}"
                ufw allow "$dns_port/tcp" comment "Mesh DNS TCP"
                ufw allow "$dns_port/udp" comment "Mesh DNS UDP"
                ;;
        esac
    done
    
    log_success "Configured UFW rules"
}

configure_firewalld() {
    log_info "Configuring firewalld..."
    
    # Gossip port
    firewall-cmd --permanent --add-port="$GOSSIP_PORT/tcp"
    
    for idx in "${COMPONENTS[@]}"; do
        case $idx in
            0)  # Agent
                firewall-cmd --permanent --add-port=18081/tcp
                ;;
            1)  # Edge
                local http_port="${EDGE_LISTEN_HTTP#:}"
                firewall-cmd --permanent --add-port="$http_port/tcp"
                if [ "$EDGE_ENABLE_HTTPS" = "true" ]; then
                    local https_port="${EDGE_LISTEN_HTTPS#:}"
                    firewall-cmd --permanent --add-port="$https_port/tcp"
                fi
                ;;
            2)  # DNS
                local dns_port="${DNS_LISTEN#:}"
                firewall-cmd --permanent --add-port="$dns_port/tcp"
                firewall-cmd --permanent --add-port="$dns_port/udp"
                ;;
        esac
    done
    
    firewall-cmd --reload
    log_success "Configured firewalld rules"
}

configure_iptables() {
    log_info "Configuring iptables..."
    
    # Gossip port
    iptables -A INPUT -p tcp --dport "$GOSSIP_PORT" -j ACCEPT
    
    for idx in "${COMPONENTS[@]}"; do
        case $idx in
            0)  # Agent
                iptables -A INPUT -p tcp --dport 18081 -j ACCEPT
                ;;
            1)  # Edge
                local http_port="${EDGE_LISTEN_HTTP#:}"
                iptables -A INPUT -p tcp --dport "$http_port" -j ACCEPT
                if [ "$EDGE_ENABLE_HTTPS" = "true" ]; then
                    local https_port="${EDGE_LISTEN_HTTPS#:}"
                    iptables -A INPUT -p tcp --dport "$https_port" -j ACCEPT
                fi
                ;;
            2)  # DNS
                local dns_port="${DNS_LISTEN#:}"
                iptables -A INPUT -p tcp --dport "$dns_port" -j ACCEPT
                iptables -A INPUT -p udp --dport "$dns_port" -j ACCEPT
                ;;
        esac
    done

    log_warning "iptables rules are not persistent. Consider saving them with 'iptables-save'."
    log_success "Configured iptables rules"
}

print_firewall_requirements() {
    echo ""
    log_info "Required firewall ports:"
    echo "  - TCP $GOSSIP_PORT (gossip)"
    
    for idx in "${COMPONENTS[@]}"; do
        case $idx in
            0)
                echo "  - TCP 18081 (agent verification)"
                ;;
            1)
                echo "  - TCP ${EDGE_LISTEN_HTTP#:} (edge HTTP)"
                if [ "$EDGE_ENABLE_HTTPS" = "true" ]; then
                    echo "  - TCP ${EDGE_LISTEN_HTTPS#:} (edge HTTPS)"
                fi
                ;;
            2)
                echo "  - TCP/UDP ${DNS_LISTEN#:} (DNS)"
                ;;
        esac
    done
}

#------------------------------------------------------------------------------
# Summary and Next Steps
#------------------------------------------------------------------------------

print_summary() {
    echo ""
    echo -e "${GREEN}═══════════════════════════════════════════════════════════════════${NC}"
    echo -e "${GREEN}                    Installation Complete!                         ${NC}"
    echo -e "${GREEN}═══════════════════════════════════════════════════════════════════${NC}"
    echo ""

    echo -e "${CYAN}Installed Components:${NC}"
    for idx in "${COMPONENTS[@]}"; do
        case $idx in
            0)
                echo "  • meshagent"
                echo "    - Config: $CONFIG_DIR/agent.yaml"
                echo "    - Data: $DATA_DIR/agent"
                echo "    - Service: meshagent.service"
                echo "    - Peer ID: $AGENT_PEER_ID"
                ;;
            1)
                echo "  • meshproxy"
                echo "    - Config: $CONFIG_DIR/edge.yaml"
                echo "    - Data: $DATA_DIR/edge"
                echo "    - Service: meshproxy.service"
                echo "    - Peer ID: $EDGE_PEER_ID"
                ;;
            2)
                echo "  • meshdns"
                echo "    - Config: $CONFIG_DIR/dns.yaml"
                echo "    - Data: $DATA_DIR/dns"
                echo "    - Service: meshdns.service"
                echo "    - Peer ID: $DNS_PEER_ID"
                ;;
        esac
        echo ""
    done

    if [ "$IS_FIRST_NODE" = "y" ]; then
        echo -e "${YELLOW}Bootstrap Address for Other Nodes:${NC}"
        local public_ip=""
        for idx in "${COMPONENTS[@]}"; do
            case $idx in
                0) public_ip="$AGENT_PUBLIC_IP"; peer_id="$AGENT_PEER_ID" ;;
                1) public_ip="$EDGE_PUBLIC_IP"; peer_id="$EDGE_PEER_ID" ;;
                2) public_ip="$AGENT_PUBLIC_IP"; peer_id="$DNS_PEER_ID" ;;  # DNS doesn't advertise IP
            esac
            if [ -n "$public_ip" ]; then
                echo "  /ip4/$public_ip/tcp/$GOSSIP_PORT/p2p/$peer_id"
                break
            fi
        done
        echo ""
    fi

    echo -e "${CYAN}Service Management Commands:${NC}"
    for idx in "${COMPONENTS[@]}"; do
        local service
        case $idx in
            0) service="meshagent" ;;
            1) service="meshproxy" ;;
            2) service="meshdns" ;;
        esac
        echo "  # Start $service"
        echo "  sudo systemctl start $service"
        echo ""
        echo "  # View logs"
        echo "  sudo journalctl -u $service -f"
        echo ""
    done

    echo -e "${CYAN}Verification:${NC}"
    for idx in "${COMPONENTS[@]}"; do
        case $idx in
            0)
                echo "  # Check agent health"
                echo "  curl http://localhost:18081/.mesh/verify"
                ;;
            1)
                echo "  # Check edge proxy health"
                local http_port="${EDGE_LISTEN_HTTP#:}"
                echo "  curl http://localhost:$http_port/health"
                ;;
            2)
                echo "  # Test DNS"
                echo "  dig @localhost $DNS_ZONE SOA"
                ;;
        esac
        echo ""
    done

    echo -e "${YELLOW}Next Steps:${NC}"
    echo "  1. Start the services: sudo systemctl start <service>"
    echo "  2. Check logs for successful startup"
    echo "  3. Verify gossip connectivity (look for 'peer connected')"
    if [ "$IS_FIRST_NODE" = "y" ]; then
        echo "  4. Share the bootstrap address with other nodes"
    fi
    echo ""
}

start_services_prompt() {
    echo ""
    if prompt_yes_no "Start the installed services now?" "y" | grep -q "y"; then
        for idx in "${COMPONENTS[@]}"; do
            local service
            case $idx in
                0) service="meshagent" ;;
                1) service="meshproxy" ;;
                2) service="meshdns" ;;
            esac
            
            log_info "Starting $service..."
            if systemctl start "$service"; then
                log_success "$service started successfully"
            else
                log_error "Failed to start $service"
                log_info "Check logs with: journalctl -u $service -f"
            fi
        done
    fi
}

#------------------------------------------------------------------------------
# Main Installation Flow
#------------------------------------------------------------------------------

main() {
    print_banner

    # Check prerequisites
    check_root
    check_dependencies

    # Component selection
    echo ""
    log_info "Select components to install:"
    local component_indices
    component_indices=$(prompt_multi_choice "Which components do you want to install?" \
        "meshagent (Backend agent - runs on RPC nodes)" \
        "meshproxy (Edge proxy - routes client requests)" \
        "meshdns (Authoritative DNS server)")
    
    read -ra COMPONENTS <<< "$component_indices"
    
    if [ ${#COMPONENTS[@]} -eq 0 ]; then
        log_error "No components selected. Exiting."
        exit 1
    fi

    echo ""
    log_info "Selected components:"
    for idx in "${COMPONENTS[@]}"; do
        case $idx in
            0) echo "  • meshagent" ;;
            1) echo "  • meshproxy" ;;
            2) echo "  • meshdns" ;;
        esac
    done

    # Install binaries
    echo ""
    install_binaries

    # Create user and directories
    create_user
    create_directories

    # Collect common configuration
    collect_common_config

    # Collect component-specific configuration
    for idx in "${COMPONENTS[@]}"; do
        case $idx in
            0) collect_agent_config ;;
            1) collect_edge_config ;;
            2) collect_dns_config ;;
        esac
    done

    # Generate configuration files
    echo ""
    log_info "Generating configuration files..."
    for idx in "${COMPONENTS[@]}"; do
        case $idx in
            0) generate_agent_config ;;
            1) generate_edge_config ;;
            2) generate_dns_config ;;
        esac
    done

    # Create systemd services
    echo ""
    log_info "Creating systemd services..."
    for idx in "${COMPONENTS[@]}"; do
        case $idx in
            0) create_agent_service ;;
            1) create_edge_service ;;
            2) create_dns_service ;;
        esac
    done

    # Enable services
    enable_services

    # Configure firewall
    configure_firewall

    # Print summary
    print_summary

    # Optionally start services
    start_services_prompt

    echo ""
    log_success "Installation complete!"
}

# Run main function
main "$@"
