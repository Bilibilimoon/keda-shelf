package main

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type ctxKey int

const userKey ctxKey = 1

type server struct {
	cfg       *Config
	users     *UserStore
	configDir string
	dataDir   string
	lib       *library
	limiter   *rateLimiter
	roots     []string
	port      int
	started   time.Time
	tlsOn     bool
	meta      *metaStore
	scrape    scraper
	// scrapeProxy is reused for manual cover fetches so image URLs reachable
	// only through the configured proxy also work from the admin page.
	scrapeProxy string

	scanMu   sync.Mutex
	scanning bool
	prog     *scanProgress
	lastScan *scanResult
	// scanEvery is the periodic incremental rescan period; zero disables it.
	scanEvery time.Duration

	// cleanMu guards lastClean, the timestamp of the last stale-cover sweep.
	cleanMu   sync.Mutex
	lastClean time.Time

	autoMu      sync.Mutex
	autoRunning bool
	autoDone    int
	autoTotal   int
	autoStop    bool
}

func userFrom(r *http.Request) *User {
	if u, ok := r.Context().Value(userKey).(*User); ok {
		return u
	}
	return nil
}

func isAdmin(r *http.Request) bool {
	u := userFrom(r)
	return u != nil && u.Gateway && u.Admin
}

// requireAuth authenticates requests that arrive on the TCP port (used by the
// iOS app). RFC 7617 Basic Auth keeps it compatible with OPDS clients.
func (s *server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		ip := clientIP(r)
		if s.limiter.blocked(ip) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
			return
		}
		name, pass, ok := r.BasicAuth()
		if !ok || name == "" {
			s.challenge(w)
			return
		}
		u := s.users.find(name)
		if u == nil || bcrypt.CompareHashAndPassword([]byte(u.Hash), []byte(pass)) != nil {
			s.limiter.fail(ip)
			s.challenge(w)
			return
		}
		s.limiter.reset(ip)
		// Accounts from users.json are read-only clients. Only a NAS admin
		// signed in through the fnOS gateway may manage the library.
		client := &User{Name: u.Name, Hash: u.Hash, Restricted: u.Restricted, Dirs: u.Dirs, Admin: false, Gateway: false}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, client)))
	})
}

// gatewayAuth trusts requests arriving on the app's Unix socket: fnOS's gateway
// has already validated the NAS session and forwards the user headers.
func (s *server) gatewayAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if name := strings.TrimSpace(r.Header.Get("X-Trim-Username")); name != "" {
			u := &User{
				Name:    name,
				Admin:   strings.EqualFold(r.Header.Get("X-Trim-Isadmin"), "true"),
				Gateway: true,
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
			return
		}
		s.requireAuth(next).ServeHTTP(w, r)
	})
}

func (s *server) challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="可达书柜", charset="UTF-8"`)
	http.Error(w, "authentication required", http.StatusUnauthorized)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type rateLimiter struct {
	mu        sync.Mutex
	failures  map[string][]time.Time
	window    time.Duration
	maxFailed int
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{failures: map[string][]time.Time{}, window: time.Minute, maxFailed: 3}
}

func (l *rateLimiter) prune(ip string, now time.Time) []time.Time {
	cutoff := now.Add(-l.window)
	kept := l.failures[ip][:0]
	for _, t := range l.failures[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	l.failures[ip] = kept
	return kept
}

func (l *rateLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(ip, time.Now())) >= l.maxFailed
}

func (l *rateLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures[ip] = append(l.failures[ip], time.Now())
}

func (l *rateLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
}
