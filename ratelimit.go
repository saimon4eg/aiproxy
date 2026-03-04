package main

import (
	"net"
	"net/http"
	"sync"
	"time"
)

type rateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitor
	rate     int
	window   time.Duration
}

type visitor struct {
	count       int
	windowStart time.Time
}

func newRateLimiter(rate int, window time.Duration) *rateLimiter {
	rl := &rateLimiter{
		visitors: make(map[string]*visitor),
		rate:     rate,
		window:   window,
	}
	// Background reaper: remove stale entries every window period.
	go func() {
		for range time.Tick(window) {
			rl.mu.Lock()
			now := time.Now()
			for ip, v := range rl.visitors {
				if now.Sub(v.windowStart) > rl.window {
					delete(rl.visitors, ip)
				}
			}
			rl.mu.Unlock()
		}
	}()
	return rl
}

func (rl *rateLimiter) allow(host string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	v, exists := rl.visitors[host]
	now := time.Now()
	if !exists || now.Sub(v.windowStart) > rl.window {
		rl.visitors[host] = &visitor{count: 1, windowStart: now}
		return true
	}
	if v.count >= rl.rate {
		return false
	}
	v.count++
	return true
}

func rateLimitMiddleware(next http.Handler) http.Handler {
	limiter := newRateLimiter(100, time.Second)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Exclude health-check endpoint — docker healthcheck must not hit the limit.
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		if !limiter.allow(host) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"type":"rate_limit","message":"too many requests"}}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
