package udp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DefaultSessionIdleTimeout = 60 * time.Second
	DefaultSweeperInterval    = 10 * time.Second
	DefaultMaxSessions        = 256
	DefaultMaxSessionsPerIP   = 32
	DefaultSessionCreateRate  = 50
)

// SessionLimits bounds UDP state created by unauthenticated packet sources.
type SessionLimits struct {
	MaxSessions        int
	MaxSessionsPerIP   int
	MaxCreatePerSecond int
}

// SessionKey identifies an inbound client connection on Relay
type SessionKey struct {
	DeviceID   string
	PublicPort uint16
	ClientAddr netip.AddrPort
}

// UDPSession holds state for an active UDP connection
type UDPSession struct {
	ID         uint32
	DeviceID   string
	PublicPort uint16
	ClientAddr netip.AddrPort
	Conn       *net.UDPConn // Local UDP socket (used by Agent to talk to 127.0.0.1:3389)
	LastSeen   atomic.Int64 // UnixNano timestamp of last activity
	closed     atomic.Bool
	onClose    func(s *UDPSession)
}

// UpdateActivity refreshes the session's last seen timestamp
func (s *UDPSession) UpdateActivity() {
	s.LastSeen.Store(time.Now().UnixNano())
}

// IsExpired checks if the session has been idle longer than timeout
func (s *UDPSession) IsExpired(timeout time.Duration) bool {
	last := time.Unix(0, s.LastSeen.Load())
	return time.Since(last) > timeout
}

// Close terminates the session and its underlying socket
func (s *UDPSession) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	if s.onClose != nil {
		s.onClose(s)
	}
	if s.Conn != nil {
		return s.Conn.Close()
	}
	return nil
}

// SessionManager manages bidirectional UDP session lookups and lifecycle
type SessionManager struct {
	mu              sync.RWMutex
	byID            map[uint32]*UDPSession
	byKey           map[SessionKey]*UDPSession
	sessionsPerIP   map[netip.Addr]int
	idleTimeout     time.Duration
	onSessionEnd    func(s *UDPSession)
	limits          SessionLimits
	rateWindowStart time.Time
	rateWindowCount int
	closed          bool
}

// NewSessionManager creates a SessionManager
func NewSessionManager(idleTimeout time.Duration, onSessionEnd func(s *UDPSession)) *SessionManager {
	return NewSessionManagerWithLimits(idleTimeout, onSessionEnd, SessionLimits{})
}

// NewSessionManagerWithLimits creates a SessionManager with explicit limits.
// Non-positive values use secure defaults rather than disabling protection.
func NewSessionManagerWithLimits(idleTimeout time.Duration, onSessionEnd func(s *UDPSession), limits SessionLimits) *SessionManager {
	if idleTimeout <= 0 {
		idleTimeout = DefaultSessionIdleTimeout
	}
	if limits.MaxSessions <= 0 {
		limits.MaxSessions = DefaultMaxSessions
	}
	if limits.MaxSessionsPerIP <= 0 {
		limits.MaxSessionsPerIP = DefaultMaxSessionsPerIP
	}
	if limits.MaxCreatePerSecond <= 0 {
		limits.MaxCreatePerSecond = DefaultSessionCreateRate
	}
	return &SessionManager{
		byID:          make(map[uint32]*UDPSession),
		byKey:         make(map[SessionKey]*UDPSession),
		sessionsPerIP: make(map[netip.Addr]int),
		idleTimeout:   idleTimeout,
		onSessionEnd:  onSessionEnd,
		limits:        limits,
	}
}

// GetByID looks up an existing session by ID
func (m *SessionManager) GetByID(id uint32) *UDPSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess := m.byID[id]
	if sess != nil {
		sess.UpdateActivity()
	}
	return sess
}

// GetByKey looks up an existing session by its SessionKey
func (m *SessionManager) GetByKey(key SessionKey) *UDPSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess := m.byKey[key]
	if sess != nil {
		sess.UpdateActivity()
	}
	return sess
}

// GetOrCreateByKey retrieves existing session or creates a new one with a random unique session ID
func (m *SessionManager) GetOrCreateByKey(key SessionKey) (*UDPSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if sess, exists := m.byKey[key]; exists {
		sess.UpdateActivity()
		return sess, false
	}
	if m.closed || len(m.byID) >= m.limits.MaxSessions {
		return nil, false
	}
	clientIP := key.ClientAddr.Addr().Unmap()
	if clientIP.IsValid() && m.sessionsPerIP[clientIP] >= m.limits.MaxSessionsPerIP {
		return nil, false
	}
	if !m.allowCreateLocked(time.Now()) {
		return nil, false
	}

	// Generate random non-zero session ID
	var id uint32
	for {
		var buf [4]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return nil, false
		}
		id = binary.BigEndian.Uint32(buf[:]) & 0x7fffffff
		if id != 0 && m.byID[id] == nil {
			break
		}
	}

	sess := &UDPSession{
		ID:         id,
		DeviceID:   key.DeviceID,
		PublicPort: key.PublicPort,
		ClientAddr: key.ClientAddr,
		onClose:    m.onSessionEnd,
	}
	sess.UpdateActivity()

	m.byID[id] = sess
	m.byKey[key] = sess
	if clientIP.IsValid() {
		m.sessionsPerIP[clientIP]++
	}
	return sess, true
}

// AddSessionWithID adds a session with a pre-assigned ID (used on Agent side upon implicit creation)
func (m *SessionManager) AddSessionWithID(id uint32, conn *net.UDPConn) *UDPSession {
	m.mu.Lock()
	defer m.mu.Unlock()

	if sess, exists := m.byID[id]; exists {
		sess.UpdateActivity()
		return sess
	}
	if id == 0 || m.closed || len(m.byID) >= m.limits.MaxSessions || !m.allowCreateLocked(time.Now()) {
		return nil
	}

	sess := &UDPSession{
		ID:      id,
		Conn:    conn,
		onClose: m.onSessionEnd,
	}
	sess.UpdateActivity()
	m.byID[id] = sess
	return sess
}

// Remove removes and closes a session
func (m *SessionManager) Remove(id uint32) {
	m.mu.Lock()
	sess := m.byID[id]
	if sess != nil {
		delete(m.byID, id)
		key := SessionKey{
			DeviceID:   sess.DeviceID,
			PublicPort: sess.PublicPort,
			ClientAddr: sess.ClientAddr,
		}
		delete(m.byKey, key)
		clientIP := sess.ClientAddr.Addr().Unmap()
		if clientIP.IsValid() {
			if count := m.sessionsPerIP[clientIP]; count > 1 {
				m.sessionsPerIP[clientIP] = count - 1
			} else {
				delete(m.sessionsPerIP, clientIP)
			}
		}
	}
	m.mu.Unlock()

	if sess != nil {
		sess.Close()
	}
}

// Close atomically detaches and closes all sessions. Closing their sockets
// wakes goroutines blocked in UDP reads during reconnect or shutdown.
func (m *SessionManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	sessions := make([]*UDPSession, 0, len(m.byID))
	for _, sess := range m.byID {
		sessions = append(sessions, sess)
	}
	m.byID = make(map[uint32]*UDPSession)
	m.byKey = make(map[SessionKey]*UDPSession)
	m.sessionsPerIP = make(map[netip.Addr]int)
	m.mu.Unlock()

	var firstErr error
	for _, sess := range sessions {
		if err := sess.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *SessionManager) allowCreateLocked(now time.Time) bool {
	if m.rateWindowStart.IsZero() || now.Sub(m.rateWindowStart) >= time.Second {
		m.rateWindowStart = now
		m.rateWindowCount = 0
	}
	if m.rateWindowCount >= m.limits.MaxCreatePerSecond {
		return false
	}
	m.rateWindowCount++
	return true
}

// ActiveCount returns current active session count
func (m *SessionManager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.byID)
}

// StartSweeper starts periodic background scan for idle sessions
func (m *SessionManager) StartSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultSweeperInterval
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.sweep()
			}
		}
	}()
}

func (m *SessionManager) sweep() {
	var expired []*UDPSession

	m.mu.RLock()
	for _, sess := range m.byID {
		if sess.IsExpired(m.idleTimeout) {
			expired = append(expired, sess)
		}
	}
	m.mu.RUnlock()

	for _, sess := range expired {
		m.Remove(sess.ID)
	}
}
