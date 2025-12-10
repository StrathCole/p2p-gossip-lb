package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidHMAC    = errors.New("invalid HMAC signature")
	ErrNotAllowlisted = errors.New("node not in allowlist")
	ErrTokenExpired   = errors.New("token expired")
)

// HMACValidator verifies HMAC signatures on messages.
type HMACValidator struct {
	secret []byte
}

// NewHMACValidator creates a validator with the given shared secret.
func NewHMACValidator(secret string) *HMACValidator {
	return &HMACValidator{secret: []byte(secret)}
}

// Sign generates an HMAC signature for the message.
func (v *HMACValidator) Sign(message []byte) string {
	mac := hmac.New(sha256.New, v.secret)
	mac.Write(message)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks if the signature matches the message.
func (v *HMACValidator) Verify(message []byte, signature string) error {
	expected := v.Sign(message)
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return ErrInvalidHMAC
	}
	return nil
}

// Allowlist manages a set of permitted node IDs.
type Allowlist struct {
	mu      sync.RWMutex
	allowed map[string]bool
}

// NewAllowlist creates an allowlist with the given initial IDs.
func NewAllowlist(ids []string) *Allowlist {
	al := &Allowlist{allowed: make(map[string]bool, len(ids))}
	for _, id := range ids {
		al.allowed[strings.TrimSpace(id)] = true
	}
	return al
}

// Add inserts a node ID into the allowlist.
func (al *Allowlist) Add(id string) {
	al.mu.Lock()
	defer al.mu.Unlock()
	al.allowed[id] = true
}

// Remove deletes a node ID from the allowlist.
func (al *Allowlist) Remove(id string) {
	al.mu.Lock()
	defer al.mu.Unlock()
	delete(al.allowed, id)
}

// Check verifies if a node ID is in the allowlist.
func (al *Allowlist) Check(id string) error {
	al.mu.RLock()
	defer al.mu.RUnlock()
	if !al.allowed[id] {
		return ErrNotAllowlisted
	}
	return nil
}

// List returns a sorted slice of all allowed node IDs.
func (al *Allowlist) List() []string {
	al.mu.RLock()
	defer al.mu.RUnlock()
	list := make([]string, 0, len(al.allowed))
	for id := range al.allowed {
		list = append(list, id)
	}
	return list
}

// Token represents a time-limited authentication token.
type Token struct {
	Payload   string
	IssuedAt  time.Time
	ExpiresAt time.Time
	Signature string
}

// TokenManager issues and validates tokens.
type TokenManager struct {
	validator *HMACValidator
	ttl       time.Duration
}

// NewTokenManager creates a token manager with the given secret and TTL.
func NewTokenManager(secret string, ttl time.Duration) *TokenManager {
	return &TokenManager{
		validator: NewHMACValidator(secret),
		ttl:       ttl,
	}
}

// Issue creates a signed token for the payload.
func (tm *TokenManager) Issue(payload string) *Token {
	now := time.Now()
	tok := &Token{
		Payload:   payload,
		IssuedAt:  now,
		ExpiresAt: now.Add(tm.ttl),
	}
	message := payload + "|" + tok.IssuedAt.Format(time.RFC3339) + "|" + tok.ExpiresAt.Format(time.RFC3339)
	tok.Signature = tm.validator.Sign([]byte(message))
	return tok
}

// Validate checks token signature and expiry.
func (tm *TokenManager) Validate(tok *Token) error {
	if time.Now().After(tok.ExpiresAt) {
		return ErrTokenExpired
	}
	message := tok.Payload + "|" + tok.IssuedAt.Format(time.RFC3339) + "|" + tok.ExpiresAt.Format(time.RFC3339)
	return tm.validator.Verify([]byte(message), tok.Signature)
}
