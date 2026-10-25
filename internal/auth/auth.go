// Package auth implements password hashing, session tokens and login rate limiting.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (RFC 9106 second recommended option, lighter on memory for small VPS).
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 2
	argonKeyLen  = 32
	saltLen      = 16
)

// MinPasswordLen is the shortest accepted admin password.
const MinPasswordLen = 10

// ErrBadHash means a stored hash could not be parsed.
var ErrBadHash = errors.New("некорректный формат хэша пароля")

// HashPassword returns an encoded Argon2id hash.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// VerifyPassword checks password against an encoded Argon2id hash in constant time.
func VerifyPassword(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrBadHash
	}
	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return false, ErrBadHash
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return false, ErrBadHash
	}
	want, err := enc.DecodeString(parts[5])
	if err != nil {
		return false, ErrBadHash
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NewToken returns a random session token and the hash to store for it.
// Only the hash is persisted, so a leaked database does not leak live sessions.
func NewToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

// HashToken hashes a session token for storage and lookup.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Limiter blocks a key after too many failures inside a window.
type Limiter struct {
	mu       sync.Mutex
	max      int
	window   time.Duration
	lockout  time.Duration
	failures map[string]*bucket
	now      func() time.Time
}

type bucket struct {
	count       int
	first       time.Time
	lockedUntil time.Time
}

// NewLimiter allows max failures per window, then blocks the key for lockout.
func NewLimiter(max int, window, lockout time.Duration) *Limiter {
	return &Limiter{max: max, window: window, lockout: lockout, failures: map[string]*bucket{}, now: time.Now}
}

// Allowed reports whether key may try again, and if not, for how long it is blocked.
func (l *Limiter) Allowed(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.failures[key]
	if b == nil {
		return true, 0
	}
	now := l.now()
	if now.Before(b.lockedUntil) {
		return false, b.lockedUntil.Sub(now)
	}
	return true, 0
}

// Fail records a failed attempt for key.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.failures[key]
	if b == nil || now.Sub(b.first) > l.window {
		b = &bucket{first: now}
		l.failures[key] = b
	}
	b.count++
	if b.count >= l.max {
		b.lockedUntil = now.Add(l.lockout)
		b.count = 0
		b.first = now
	}
}

// Reset clears failures for key after a successful login.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}
