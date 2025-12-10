package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lunc/mesh/pkg/registry"
	"github.com/lunc/mesh/pkg/selector"
)

func defaultTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   128,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

func extractChain(r *http.Request) (registry.ChainID, string, error) {
	host := stripPort(r.Host)
	if host == "" {
		host = stripPort(r.URL.Host)
	}
	host = strings.TrimSuffix(host, ".")
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		chain := r.Header.Get("X-Mesh-Chain")
		if chain == "" {
			return "", "", fmt.Errorf("no chain in host")
		}
		return registry.ChainID(chain), "", nil
	}
	service := strings.ToLower(parts[0])
	chain := registry.ChainID(parts[1])
	return chain, service, nil
}

func stripPort(host string) string {
	if host == "" {
		return ""
	}
	if strings.HasPrefix(host, "[") {
		if idx := strings.LastIndex(host, ":"); idx != -1 {
			return strings.Trim(host[:idx], "[]")
		}
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func clientIPFromRequest(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			ip := strings.TrimSpace(parts[0])
			if ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isWebsocketRequest(r *http.Request) bool {
	connection := strings.ToLower(r.Header.Get("Connection"))
	upgrade := strings.ToLower(r.Header.Get("Upgrade"))
	return strings.Contains(connection, "upgrade") && upgrade == "websocket"
}

func buildRequestContext(r *http.Request, chain registry.ChainID, needWS bool) selector.RequestContext {
	queryCopy := r.URL.Query()
	height := parseHeight(queryCopy)
	var clientIP net.IP
	if ip := clientIPFromRequest(r); ip != "" {
		clientIP = net.ParseIP(ip)
	}
	return selector.RequestContext{
		ChainID:  chain,
		Method:   r.Method,
		Path:     r.URL.Path,
		Query:    queryCopy,
		NeedWS:   needWS,
		Height:   height,
		ClientIP: clientIP,
		APIKey:   r.Header.Get("X-Mesh-Key"),
		JWTSub:   extractJWTSubject(r.Header.Get("Authorization")),
	}
}

func parseHeight(values url.Values) *int64 {
	raw := values.Get("height")
	if raw == "" {
		return nil
	}
	height, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || height <= 0 {
		return nil
	}
	return &height
}

func extractJWTSubject(header string) string {
	const prefix = "Bearer "
	if header == "" || !strings.HasPrefix(header, prefix) {
		return ""
	}
	token := strings.TrimSpace(header[len(prefix):])
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Sub
}

func readRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(data))
	return data, nil
}

func buildOutboundRequest(ctx context.Context, original *http.Request, body []byte, targetHost, scheme string) (*http.Request, error) {
	target := *original.URL
	target.Host = targetHost
	target.Scheme = scheme

	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}

	outbound, err := http.NewRequestWithContext(ctx, original.Method, target.String(), reader)
	if err != nil {
		return nil, err
	}
	outbound.Header = cloneHeader(original.Header)
	sanitizeHopHeaders(outbound.Header)
	outbound.Host = targetHost

	outbound.Header.Set("X-Forwarded-Host", stripPort(original.Host))
	outbound.Header.Set("X-Forwarded-Proto", requestProto(original))

	forwarded := appendForwardedFor(original.Header.Get("X-Forwarded-For"), clientIPFromRequest(original))
	if forwarded != "" {
		outbound.Header.Set("X-Forwarded-For", forwarded)
	}

	if ip := clientIPFromRequest(original); ip != "" {
		outbound.Header.Set("X-Real-IP", ip)
	}

	return outbound, nil
}

func requestProto(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto
	}
	return "http"
}

func appendForwardedFor(existing, ip string) string {
	if ip == "" {
		return existing
	}
	if existing == "" {
		return ip
	}
	return existing + ", " + ip
}

func cloneHeader(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

var hopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func sanitizeHopHeaders(header http.Header) {
	for _, key := range hopHeaders {
		header.Del(key)
	}
}

func cacheKey(r *http.Request, suffix string) string {
	var builder strings.Builder
	builder.WriteString(r.Method)
	builder.WriteString("|")
	builder.WriteString(stripPort(r.Host))
	builder.WriteString("|")
	builder.WriteString(r.URL.Path)

	query := r.URL.Query()
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values := append([]string(nil), query[key]...)
		sort.Strings(values)
		for _, value := range values {
			builder.WriteString("|")
			builder.WriteString(key)
			builder.WriteString("=")
			builder.WriteString(value)
		}
	}
	if suffix != "" {
		builder.WriteString("|")
		builder.WriteString(suffix)
	}
	return builder.String()
}

func negativeCacheKey(r *http.Request) string {
	return cacheKey(r, "404")
}

func shouldNegativeCache(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	path := strings.ToLower(r.URL.Path)
	return strings.Contains(path, "/txs/") || strings.Contains(path, "/tx/") || strings.Contains(path, "tx_search")
}
