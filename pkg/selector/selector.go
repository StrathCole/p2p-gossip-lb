package selector

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"math"
	"math/big"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/lunc/mesh/pkg/registry"
)

// ErrNoBackend is returned when no candidates remain after filtering.
var ErrNoBackend = errors.New("selector: no backend candidates")

// RequestContext summarises the inputs required to place a request.
type RequestContext struct {
	ChainID  registry.ChainID
	Method   string
	Path     string
	Query    url.Values
	NeedWS   bool
	Height   *int64
	ClientIP net.IP
	APIKey   string
	JWTSub   string
}

// Environment carries chain-wide or mesh-wide state that influences selection.
type Environment struct {
	BestHeight int64
}

// Candidate represents a backend with the associated runtime measurements.
type Candidate struct {
	Meta       registry.BackendMeta
	Metrics    registry.BackendMetrics
	GeoPenalty float64
	Score      float64
	Weight     float64
}

// ScoreWeights controls the contribution of each signal to the score.
type ScoreWeights struct {
	RTT       float64
	P95       float64
	CPU       float64
	Geo       float64
	FailRate  float64
	HeightLag float64
}

// Config tunes the selector behaviour.
type Config struct {
	Weights                 ScoreWeights
	HeightDelta             int64
	SoftmaxTemperature      float64
	StickinessProbability   float64
	CacheableHeightBias     float64
	StickyFallbackRandomize bool
}

// DefaultConfig returns opinionated defaults based on the service design.
func DefaultConfig() Config {
	return Config{
		Weights: ScoreWeights{
			RTT:       0.35,
			P95:       0.25,
			CPU:       0.15,
			Geo:       0.10,
			FailRate:  0.10,
			HeightLag: 0.05,
		},
		HeightDelta:             2,
		SoftmaxTemperature:      0.7,
		StickinessProbability:   0.8,
		CacheableHeightBias:     -0.2,
		StickyFallbackRandomize: true,
	}
}

// Pick selects a backend using the configured scoring and stickiness rules.
func Pick(ctx RequestContext, env Environment, pool []Candidate, cfg Config) (Candidate, error) {
	cand := filterCandidates(ctx, env, pool, cfg)
	if len(cand) == 0 {
		return Candidate{}, ErrNoBackend
	}

	bestHeight := env.BestHeight
	for i := range cand {
		metrics := cand[i].Metrics
		lag := float64(maxInt64(0, bestHeight-metrics.Height))

		c := &cand[i]
		c.Score = cfg.Weights.RTT*float64(metrics.RTTms) +
			cfg.Weights.P95*float64(metrics.P95ms) +
			cfg.Weights.CPU*float64(metrics.CPU) +
			cfg.Weights.Geo*c.GeoPenalty +
			cfg.Weights.FailRate*float64(metrics.FailRate) +
			cfg.Weights.HeightLag*lag

		c.Score += methodBias(ctx, metrics)
		if ctx.Height != nil {
			c.Score += cfg.CacheableHeightBias
		}
	}

	weights := softmaxWeights(cand, cfg.SoftmaxTemperature)
	for i := range cand {
		cand[i].Weight = weights[i]
	}

	sticky := stickyKey(ctx)
	if sticky != "" {
		if cfg.StickinessProbability >= 1.0 || shouldStick(cfg.StickinessProbability) {
			if chosen, ok := stickyChoice(sticky, cand); ok {
				return cand[chosen], nil
			}
		}
	}

	idx := weightedChoice(weights)
	return cand[idx], nil
}

func filterCandidates(ctx RequestContext, env Environment, pool []Candidate, cfg Config) []Candidate {
	capNeeded := capabilityForRequest(ctx)
	filtered := make([]Candidate, 0, len(pool))
	minHeight := env.BestHeight - cfg.HeightDelta

	for _, candidate := range pool {
		if candidate.Meta.ChainID != ctx.ChainID {
			continue
		}
		if capNeeded != "" && !candidate.Meta.Caps[capNeeded] {
			continue
		}
		metrics := candidate.Metrics
		if strings.ToLower(metrics.Health) != "ok" {
			continue
		}
		if metrics.CatchingUp {
			continue
		}
		if metrics.Height < minHeight && ctx.Height == nil {
			continue
		}
		if ctx.Height != nil && metrics.Height < *ctx.Height {
			continue
		}
		filtered = append(filtered, candidate)
	}

	return filtered
}

func capabilityForRequest(ctx RequestContext) string {
	if ctx.NeedWS {
		return "ws"
	}

	path := strings.ToLower(ctx.Path)
	if strings.Contains(path, "grpc") {
		return "grpc"
	}
	if strings.HasPrefix(path, "/cosmos") || strings.Contains(path, "/lcd") {
		return "lcd"
	}
	return "rpc"
}

func methodBias(ctx RequestContext, metrics registry.BackendMetrics) float64 {
	method := strings.ToLower(ctx.Method)
	switch {
	case isHotMethod(method):
		return 0.1 * float64(metrics.RTTms)
	case isHeavyMethod(method):
		return 0.1 * float64(metrics.CPU)
	default:
		return 0
	}
}

func isHotMethod(method string) bool {
	hot := []string{"abci_query", "tx", "tx_search"}
	for _, name := range hot {
		if strings.HasPrefix(method, name) {
			return true
		}
	}
	return false
}

func isHeavyMethod(method string) bool {
	heavy := []string{"block", "block_by_hash", "validators"}
	for _, name := range heavy {
		if strings.HasPrefix(method, name) {
			return true
		}
	}
	return false
}

func softmaxWeights(candidates []Candidate, temperature float64) []float64 {
	if temperature <= 0 {
		temperature = 1
	}

	expScores := make([]float64, len(candidates))
	var sum float64
	minScore := candidates[0].Score
	for i := 1; i < len(candidates); i++ {
		if candidates[i].Score < minScore {
			minScore = candidates[i].Score
		}
	}

	for i, cand := range candidates {
		exp := math.Exp(-(cand.Score - minScore) / temperature)
		expScores[i] = exp
		sum += exp
	}

	if sum == 0 {
		uniform := 1.0 / float64(len(candidates))
		for i := range candidates {
			expScores[i] = uniform
		}
		return expScores
	}

	for i := range expScores {
		expScores[i] /= sum
	}
	return expScores
}

func weightedChoice(weights []float64) int {
	if len(weights) == 1 {
		return 0
	}

	r, err := randFloat64()
	if err != nil {
		return int(time.Now().UnixNano() % int64(len(weights)))
	}

	cum := 0.0
	for i, w := range weights {
		cum += w
		if r <= cum {
			return i
		}
	}
	return len(weights) - 1
}

func randFloat64() (float64, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1<<53))
	if err != nil {
		return 0, err
	}
	return float64(n.Int64()) / (1 << 53), nil
}

func stickyKey(ctx RequestContext) string {
	if ctx.APIKey != "" {
		return "key:" + ctx.APIKey
	}
	if ctx.JWTSub != "" {
		return "jwt:" + ctx.JWTSub
	}
	if ctx.ClientIP != nil {
		return "ip:" + ipBucket(ctx.ClientIP)
	}
	return ""
}

func ipBucket(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		masked := binary.BigEndian.Uint32(v4) & 0xFFFFFF00
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, masked)
		return net.IP(buf).String()
	}

	v6 := ip.To16()
	if v6 == nil {
		return "unknown"
	}
	mask := make(net.IP, len(v6))
	copy(mask, v6)
	for i := 6; i < len(mask); i++ {
		mask[i] = 0
	}
	return net.IP(mask).String()
}

func stickyChoice(key string, candidates []Candidate) (int, bool) {
	total := 0.0
	for _, c := range candidates {
		total += c.Weight
	}
	if total == 0 {
		return 0, len(candidates) > 0
	}

	hash := sipHash24(key)
	r := float64(hash%1_000_000) / 1_000_000
	cum := 0.0
	for i, c := range candidates {
		cum += c.Weight
		if r <= cum/total {
			return i, true
		}
	}
	return len(candidates) - 1, true
}

func shouldStick(prob float64) bool {
	if prob <= 0 {
		return false
	}
	if prob >= 1 {
		return true
	}

	r, err := randFloat64()
	if err != nil {
		return true
	}
	return r < prob
}

func sipHash24(key string) uint64 {
	const (
		c0 = 0x736f6d6570736575
		c1 = 0x646f72616e646f6d
		c2 = 0x6c7967656e657261
		c3 = 0x7465646279746573
	)

	b := []byte(key)
	k0 := binary.LittleEndian.Uint64([]byte("selector"))
	k1 := binary.LittleEndian.Uint64([]byte("stickeys"))

	v0 := c0 ^ k0
	v1 := c1 ^ k1
	v2 := c2 ^ k0
	v3 := c3 ^ k1

	var m uint64
	n := len(b)
	last := n - (n % 8)
	var i int

	for i = 0; i < last; i += 8 {
		m = binary.LittleEndian.Uint64(b[i:])
		v3 ^= m
		doSipRound(&v0, &v1, &v2, &v3)
		doSipRound(&v0, &v1, &v2, &v3)
		v0 ^= m
	}

	m = uint64(n) << 56
	switch n & 7 {
	case 7:
		m |= uint64(b[i+6]) << 48
		fallthrough
	case 6:
		m |= uint64(b[i+5]) << 40
		fallthrough
	case 5:
		m |= uint64(b[i+4]) << 32
		fallthrough
	case 4:
		m |= uint64(b[i+3]) << 24
		fallthrough
	case 3:
		m |= uint64(b[i+2]) << 16
		fallthrough
	case 2:
		m |= uint64(b[i+1]) << 8
		fallthrough
	case 1:
		m |= uint64(b[i])
	}

	v3 ^= m
	doSipRound(&v0, &v1, &v2, &v3)
	doSipRound(&v0, &v1, &v2, &v3)
	v0 ^= m
	v2 ^= 0xff
	doSipRound(&v0, &v1, &v2, &v3)
	doSipRound(&v0, &v1, &v2, &v3)
	doSipRound(&v0, &v1, &v2, &v3)
	doSipRound(&v0, &v1, &v2, &v3)

	return v0 ^ v1 ^ v2 ^ v3
}

func doSipRound(v0, v1, v2, v3 *uint64) {
	*v0 += *v1
	*v1 = bitsRotateLeft64(*v1, 13)
	*v1 ^= *v0
	*v0 = bitsRotateLeft64(*v0, 32)
	*v2 += *v3
	*v3 = bitsRotateLeft64(*v3, 16)
	*v3 ^= *v2
	*v0 += *v3
	*v3 = bitsRotateLeft64(*v3, 21)
	*v3 ^= *v0
	*v2 += *v1
	*v1 = bitsRotateLeft64(*v1, 17)
	*v1 ^= *v2
	*v2 = bitsRotateLeft64(*v2, 32)
}

func bitsRotateLeft64(x uint64, k int) uint64 {
	return (x << uint(k)) | (x >> uint(64-k))
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
