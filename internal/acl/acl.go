package acl

import (
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

type temporaryRule struct {
	expiresAt time.Time
}

// Manager evaluates client IP against configured ACL and temporary whitelist rules
type Manager struct {
	mu             sync.RWMutex
	defaultAllow   bool
	staticPrefixes []netip.Prefix
	tempIPs        map[netip.Addr]temporaryRule
}

// NewManager creates an ACL Manager
func NewManager(defaultPolicy string, allowList []string) (*Manager, error) {
	m := &Manager{
		defaultAllow: strings.ToLower(defaultPolicy) == "allow",
		tempIPs:      make(map[netip.Addr]temporaryRule),
	}

	for _, entry := range allowList {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !strings.Contains(entry, "/") {
			// Plain IP, append /32 or /128
			if strings.Contains(entry, ":") {
				entry += "/128"
			} else {
				entry += "/32"
			}
		}
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			return nil, err
		}
		m.staticPrefixes = append(m.staticPrefixes, prefix)
	}

	return m, nil
}

// AllowTemporary adds a temporary whitelist for an IP with a TTL (e.g. 30min)
func (m *Manager) AllowTemporary(ip netip.Addr, ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tempIPs[ip] = temporaryRule{
		expiresAt: time.Now().Add(ttl),
	}
}

// IsAllowed checks if the given remote address is authorized to connect
func (m *Manager) IsAllowed(remoteAddr net.Addr) bool {
	var addr netip.Addr
	switch a := remoteAddr.(type) {
	case *net.TCPAddr:
		if parsed, ok := netip.AddrFromSlice(a.IP); ok {
			addr = parsed.Unmap()
		}
	case *net.UDPAddr:
		if parsed, ok := netip.AddrFromSlice(a.IP); ok {
			addr = parsed.Unmap()
		}
	default:
		host, _, err := net.SplitHostPort(remoteAddr.String())
		if err == nil {
			if parsed, err := netip.ParseAddr(host); err == nil {
				addr = parsed.Unmap()
			}
		}
	}

	return m.IsAllowedAddr(addr)
}

// IsAllowedAddr is IsAllowed for a parsed address, so per-packet callers do not
// have to allocate a net.Addr just to be checked.
func (m *Manager) IsAllowedAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return m.defaultAllow
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	// 1. Check temporary rules
	if rule, exists := m.tempIPs[addr]; exists {
		if time.Now().Before(rule.expiresAt) {
			return true
		}
	}

	// 2. Check static prefixes
	for _, prefix := range m.staticPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}

	return m.defaultAllow
}
