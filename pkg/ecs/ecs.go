package ecs

import (
	"encoding/binary"
	"fmt"
	"net"

	"github.com/miekg/dns"
)

// ClientSubnet represents EDNS Client Subnet information.
type ClientSubnet struct {
	Family       uint16 // 1=IPv4, 2=IPv6
	SourcePrefix uint8  // Source prefix length
	ScopePrefix  uint8  // Scope prefix length (in response)
	Address      net.IP // Client subnet address
}

// ParseECS extracts EDNS Client Subnet from a DNS message.
func ParseECS(msg *dns.Msg) (*ClientSubnet, error) {
	opt := msg.IsEdns0()
	if opt == nil {
		return nil, nil
	}

	for _, option := range opt.Option {
		if subnet, ok := option.(*dns.EDNS0_SUBNET); ok {
			cs := &ClientSubnet{
				Family:       subnet.Family,
				SourcePrefix: subnet.SourceNetmask,
				ScopePrefix:  0, // Will be set in response
				Address:      subnet.Address,
			}
			return cs, nil
		}
	}

	return nil, nil
}

// AddECS adds EDNS Client Subnet option to a DNS message.
func AddECS(msg *dns.Msg, subnet *ClientSubnet) {
	opt := msg.IsEdns0()
	if opt == nil {
		msg.SetEdns0(4096, false)
		opt = msg.IsEdns0()
	}

	ecs := &dns.EDNS0_SUBNET{
		Code:          dns.EDNS0SUBNET,
		Family:        subnet.Family,
		SourceNetmask: subnet.SourcePrefix,
		SourceScope:   subnet.ScopePrefix,
		Address:       subnet.Address,
	}

	opt.Option = append(opt.Option, ecs)
}

// GetClientIP extracts the client IP from either ECS or the direct connection.
// Prefers ECS if available, falls back to remote address.
func GetClientIP(msg *dns.Msg, remoteAddr net.Addr) net.IP {
	// Try ECS first
	if subnet, err := ParseECS(msg); err == nil && subnet != nil {
		return subnet.Address
	}

	// Fall back to connection address
	if udpAddr, ok := remoteAddr.(*net.UDPAddr); ok {
		return udpAddr.IP
	}
	if tcpAddr, ok := remoteAddr.(*net.TCPAddr); ok {
		return tcpAddr.IP
	}

	return nil
}

// TruncateIP truncates an IP to the given prefix length for subnet matching.
func TruncateIP(ip net.IP, prefixLen uint8) net.IP {
	if ip == nil {
		return nil
	}

	// Ensure we're working with 16-byte representation
	ip16 := ip.To16()
	if ip16 == nil {
		return nil
	}

	// Determine if IPv4 or IPv6
	isIPv4 := ip.To4() != nil
	maxLen := uint8(128)
	if isIPv4 {
		maxLen = 32
	}

	if prefixLen > maxLen {
		prefixLen = maxLen
	}

	// Create mask
	mask := net.CIDRMask(int(prefixLen), int(maxLen))

	// Apply mask
	result := make(net.IP, len(ip16))
	for i := range ip16 {
		if isIPv4 && i < 12 {
			result[i] = ip16[i] // Keep IPv4-mapped prefix
		} else {
			idx := i
			if isIPv4 {
				idx = i - 12 // Adjust for IPv4-mapped offset
			}
			if idx < len(mask) {
				result[i] = ip16[i] & mask[idx]
			}
		}
	}

	if isIPv4 {
		return result.To4()
	}
	return result
}

// SubnetKey returns a string key for a subnet (for caching/mapping).
func SubnetKey(ip net.IP, prefixLen uint8) string {
	truncated := TruncateIP(ip, prefixLen)
	if truncated == nil {
		return ""
	}
	return fmt.Sprintf("%s/%d", truncated.String(), prefixLen)
}

// IPToBytes converts an IP to bytes for storage.
func IPToBytes(ip net.IP) []byte {
	if ip4 := ip.To4(); ip4 != nil {
		return ip4
	}
	return ip.To16()
}

// BytesToIP converts bytes back to an IP.
func BytesToIP(b []byte) net.IP {
	if len(b) == 4 {
		return net.IPv4(b[0], b[1], b[2], b[3])
	}
	if len(b) == 16 {
		return net.IP(b)
	}
	return nil
}

// IPToUint32 converts an IPv4 address to uint32 (network byte order).
func IPToUint32(ip net.IP) uint32 {
	ip4 := ip.To4()
	if ip4 == nil {
		return 0
	}
	return binary.BigEndian.Uint32(ip4)
}

// Uint32ToIP converts a uint32 to IPv4 address.
func Uint32ToIP(n uint32) net.IP {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, n)
	return net.IPv4(b[0], b[1], b[2], b[3])
}
