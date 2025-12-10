package circuitbreaker

import (
	"errors"
	"sync"
	"time"
)

var (
	// ErrCircuitOpen is returned when the circuit breaker is in open state.
	ErrCircuitOpen = errors.New("circuit breaker: circuit is open")
	// ErrTooManyRequests is returned when too many requests are made in half-open state.
	ErrTooManyRequests = errors.New("circuit breaker: too many requests")
)

// State represents the circuit breaker state.
type State int

const (
	// StateClosed allows all requests.
	StateClosed State = iota
	// StateOpen blocks all requests.
	StateOpen
	// StateHalfOpen allows limited requests to test if backend recovered.
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Config configures the circuit breaker behavior.
type Config struct {
	// MaxRequests is the maximum number of requests in half-open state.
	MaxRequests uint32
	// Interval is the cyclic period in closed state to clear internal counters.
	Interval time.Duration
	// Timeout is the period in open state after which it transitions to half-open.
	Timeout time.Duration
	// ReadyToTrip returns true if breaker should trip to open state.
	// Default: 5xx rate > 20% over 50 requests
	ReadyToTrip func(counts Counts) bool
	// OnStateChange is called when state transitions.
	OnStateChange func(name string, from State, to State)
}

// DefaultConfig returns sensible defaults for an HTTP backend.
func DefaultConfig() Config {
	return Config{
		MaxRequests: 5,
		Interval:    10 * time.Second,
		Timeout:     30 * time.Second,
		ReadyToTrip: func(counts Counts) bool {
			// Trip if 5xx rate > 20% and at least 50 requests
			if counts.TotalRequests < 50 {
				return false
			}
			failureRate := float64(counts.TotalFailures) / float64(counts.TotalRequests)
			return failureRate > 0.20
		},
	}
}

// Counts holds the statistics for a circuit breaker.
type Counts struct {
	Requests       uint32 // Requests in current interval
	TotalRequests  uint32 // Total requests since last reset
	TotalSuccesses uint32 // Total successes
	TotalFailures  uint32 // Total failures
	ConsecSucc     uint32 // Consecutive successes in half-open
	ConsecFail     uint32 // Consecutive failures
}

func (c *Counts) onRequest() {
	c.Requests++
	c.TotalRequests++
}

func (c *Counts) onSuccess() {
	c.TotalSuccesses++
	c.ConsecSucc++
	c.ConsecFail = 0
}

func (c *Counts) onFailure() {
	c.TotalFailures++
	c.ConsecFail++
	c.ConsecSucc = 0
}

func (c *Counts) clear() {
	c.Requests = 0
}

// CircuitBreaker protects a backend from overload by failing fast.
type CircuitBreaker struct {
	name   string
	config Config

	mu         sync.Mutex
	state      State
	generation uint64
	counts     Counts
	expiry     time.Time
}

// New creates a new circuit breaker with the given name and config.
func New(name string, config Config) *CircuitBreaker {
	cb := &CircuitBreaker{
		name:   name,
		config: config,
		state:  StateClosed,
		expiry: time.Now().Add(config.Interval),
	}

	if cb.config.ReadyToTrip == nil {
		cb.config.ReadyToTrip = DefaultConfig().ReadyToTrip
	}

	return cb
}

// Call executes the given function if the circuit allows it.
func (cb *CircuitBreaker) Call(fn func() error) error {
	generation, err := cb.beforeRequest()
	if err != nil {
		return err
	}

	defer func() {
		if e := recover(); e != nil {
			cb.afterRequest(generation, false)
			panic(e)
		}
	}()

	err = fn()
	cb.afterRequest(generation, err == nil)
	return err
}

// State returns the current circuit breaker state.
func (cb *CircuitBreaker) State() State {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()
	state, _ := cb.currentState(now)
	return state
}

// Counts returns a copy of the current counts.
func (cb *CircuitBreaker) Counts() Counts {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.counts
}

func (cb *CircuitBreaker) beforeRequest() (uint64, error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()
	state, generation := cb.currentState(now)

	if state == StateOpen {
		return generation, ErrCircuitOpen
	} else if state == StateHalfOpen && cb.counts.Requests >= cb.config.MaxRequests {
		return generation, ErrTooManyRequests
	}

	cb.counts.onRequest()
	return generation, nil
}

func (cb *CircuitBreaker) afterRequest(before uint64, success bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()
	state, generation := cb.currentState(now)

	if generation != before {
		return
	}

	if success {
		cb.onSuccess(state, now)
	} else {
		cb.onFailure(state, now)
	}
}

func (cb *CircuitBreaker) onSuccess(state State, now time.Time) {
	cb.counts.onSuccess()

	if state == StateHalfOpen {
		// Enough successes to close the circuit
		if cb.counts.ConsecSucc >= cb.config.MaxRequests {
			cb.setState(StateClosed, now)
		}
	}
}

func (cb *CircuitBreaker) onFailure(state State, now time.Time) {
	cb.counts.onFailure()

	switch state {
	case StateClosed:
		if cb.config.ReadyToTrip(cb.counts) {
			cb.setState(StateOpen, now)
		}
	case StateHalfOpen:
		cb.setState(StateOpen, now)
	}
}

func (cb *CircuitBreaker) currentState(now time.Time) (State, uint64) {
	switch cb.state {
	case StateClosed:
		if !cb.expiry.IsZero() && cb.expiry.Before(now) {
			cb.counts.clear()
			cb.expiry = now.Add(cb.config.Interval)
		}
	case StateOpen:
		if cb.expiry.Before(now) {
			cb.setState(StateHalfOpen, now)
		}
	}
	return cb.state, cb.generation
}

func (cb *CircuitBreaker) setState(state State, now time.Time) {
	if cb.state == state {
		return
	}

	prev := cb.state
	cb.state = state
	cb.generation++
	cb.counts = Counts{}

	switch state {
	case StateClosed:
		cb.expiry = now.Add(cb.config.Interval)
	case StateOpen:
		cb.expiry = now.Add(cb.config.Timeout)
	case StateHalfOpen:
		cb.expiry = time.Time{} // No expiry in half-open
	}

	if cb.config.OnStateChange != nil {
		cb.config.OnStateChange(cb.name, prev, state)
	}
}
