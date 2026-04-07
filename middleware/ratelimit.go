package middleware

import (
	"golang.org/x/time/rate"
	// "net"
	"net/http"
	// "strings"
	"sync"
	"time"
)

type RateLimiter struct {
	mu      sync.Mutex
	clients map[string]*entry
	rps     rate.Limit
	burst   int
	perIP   bool
}

type entry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func NewRateLimiter(rps float64, burst int, perIP bool) *RateLimiter {
	rl := &RateLimiter{
		clients: make(map[string]*entry),
		rps:     rate.Limit(rps),
		burst:   burst,
		perIP:   perIP,
	}
	go rl.cleanup()
	return rl
}

func (rl *RateLimiter) getLimiter(key string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	e, ok := rl.clients[key]
	if !ok {
		e = &entry{limiter: rate.NewLimiter(rl.rps, rl.burst)}
		rl.clients[key] = e
	}
	e.lastSeen = time.Now()
	return e.limiter
}

// cleanup removes clients not seen in 5 minutes
func (rl *RateLimiter) cleanup() {
	for range time.Tick(time.Minute) {
		rl.mu.Lock()
		for k, e := range rl.clients {
			if time.Since(e.lastSeen) > 5*time.Minute {
				delete(rl.clients, k)
			}
		}
		rl.mu.Unlock()
	}
}

func (rl *RateLimiter) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := "global"
			if rl.perIP {
				key = clientIP(r)
			}
			if !rl.getLimiter(key).Allow() {
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
