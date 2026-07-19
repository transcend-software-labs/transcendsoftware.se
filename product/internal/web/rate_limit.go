package web

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type attemptWindow struct {
	expires time.Time
	count   int
}

// attemptLimiter is a small fixed-window guard for authentication/email abuse.
// Forge currently runs one control-plane machine; if that changes, move these
// counters to the shared database or edge while keeping this interface.
type attemptLimiter struct {
	mu        sync.Mutex
	windows   map[string]attemptWindow
	lastSweep time.Time
}

func newAttemptLimiter() *attemptLimiter {
	return &attemptLimiter{windows: make(map[string]attemptWindow)}
}

func (l *attemptLimiter) allow(key string, limit int, window time.Duration, now time.Time) bool {
	if limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Public endpoints can see an unbounded set of attacker-controlled email
	// keys. Expire old windows opportunistically so the guard itself cannot be
	// turned into a slow memory leak.
	if l.lastSweep.IsZero() || now.Sub(l.lastSweep) >= time.Minute {
		for existingKey, existing := range l.windows {
			if !now.Before(existing.expires) {
				delete(l.windows, existingKey)
			}
		}
		l.lastSweep = now
	}
	w := l.windows[key]
	if w.expires.IsZero() || !now.Before(w.expires) {
		l.windows[key] = attemptWindow{expires: now.Add(window), count: 1}
		return true
	}
	if w.count >= limit {
		return false
	}
	w.count++
	l.windows[key] = w
	return true
}

func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("Fly-Client-IP")); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

// allowAuth applies independent per-IP and per-email ceilings. Both must pass;
// the IP guard protects provider reputation, while the address guard stops a
// distributed attacker from repeatedly mailing one victim.
func (s *Server) allowAuth(r *http.Request, action, email string, ipLimit, emailLimit int, window time.Duration) bool {
	now := time.Now().UTC()
	if !s.authLimiter.allow(action+":ip:"+clientIP(r), ipLimit, window, now) {
		return false
	}
	if email == "" {
		return true
	}
	return s.authLimiter.allow(action+":email:"+email, emailLimit, window, now)
}
