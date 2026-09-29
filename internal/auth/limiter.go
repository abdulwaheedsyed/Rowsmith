package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"sync"
	"time"
)

// Limiter is a keyed token bucket used to throttle login and MFA attempts.
type Limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewLimiter(perMinute, burst int) *Limiter {
	l := &Limiter{rate: float64(perMinute) / 60, burst: float64(burst), buckets: map[string]*bucket{}}
	go l.gc()
	return l
}

// Allow consumes a token for key, reporting false (and a retry delay) when exhausted.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	}
	b.tokens--
	return true, 0
}

func (l *Limiter) gc() {
	for range time.Tick(5 * time.Minute) {
		l.mu.Lock()
		for k, b := range l.buckets {
			if time.Since(b.last) > 30*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.mu.Unlock()
	}
}

// NewSessionToken returns a 256-bit random bearer token and the hash stored server-side.
func NewSessionToken() (token, hash string) {
	token = base64.RawURLEncoding.EncodeToString(randomBytes(32))
	return token, HashToken(token)
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func NewCSRFToken() string { return base64.RawURLEncoding.EncodeToString(randomBytes(24)) }
