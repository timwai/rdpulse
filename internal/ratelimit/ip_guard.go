package ratelimit

import (
	"net"
	"strings"
	"sync"
	"time"
)

type entry struct {
	active      int
	windowStart time.Time
	count       int
	lastSeen    time.Time
}

// IPGuard applies both a concurrent-connection bound and a fixed-window
// creation rate per remote IP. It opportunistically removes idle keys.
type IPGuard struct {
	mu            sync.Mutex
	entries       map[string]*entry
	maxConcurrent int
	maxPerWindow  int
	window        time.Duration
	operations    uint64
}

func NewIPGuard(maxConcurrent, maxPerWindow int, window time.Duration) *IPGuard {
	if maxConcurrent <= 0 {
		maxConcurrent = 16
	}
	if maxPerWindow <= 0 {
		maxPerWindow = 60
	}
	if window <= 0 {
		window = time.Minute
	}
	return &IPGuard{
		entries:       make(map[string]*entry),
		maxConcurrent: maxConcurrent,
		maxPerWindow:  maxPerWindow,
		window:        window,
	}
}

func (g *IPGuard) Acquire(addr net.Addr) (func(), bool) {
	key := addressKey(addr)
	now := time.Now()
	g.mu.Lock()
	g.operations++
	if g.operations%128 == 0 {
		g.sweepLocked(now)
	}
	e := g.entries[key]
	if e == nil {
		e = &entry{windowStart: now}
		g.entries[key] = e
	}
	if now.Sub(e.windowStart) >= g.window {
		e.windowStart = now
		e.count = 0
	}
	if e.active >= g.maxConcurrent || e.count >= g.maxPerWindow {
		e.lastSeen = now
		g.mu.Unlock()
		return nil, false
	}
	e.active++
	e.count++
	e.lastSeen = now
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if current := g.entries[key]; current != nil && current.active > 0 {
				current.active--
				current.lastSeen = time.Now()
			}
			g.mu.Unlock()
		})
	}, true
}

// Allow records one rate-limited attempt without holding a concurrency slot.
func (g *IPGuard) Allow(addr net.Addr) bool {
	release, ok := g.Acquire(addr)
	if ok {
		release()
	}
	return ok
}

func (g *IPGuard) sweepLocked(now time.Time) {
	for key, e := range g.entries {
		if e.active == 0 && now.Sub(e.lastSeen) > 2*g.window {
			delete(g.entries, key)
		}
	}
}

func addressKey(addr net.Addr) string {
	if addr == nil {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err == nil {
		return strings.Trim(host, "[]")
	}
	return addr.String()
}
