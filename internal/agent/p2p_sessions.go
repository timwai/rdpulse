package agent

import (
	"context"
	"sync"

	"rdpulse/internal/protocol"
	"rdpulse/internal/udp"
)

type p2pSession struct {
	id         uint32
	controller string
	token      []byte
	updates    chan []protocol.CandidateInfo
	ctx        context.Context
	cancel     context.CancelFunc
	replay     udp.ReplayWindow
}

type p2pSessionRegistry struct {
	mu       sync.RWMutex
	sessions map[uint32]*p2pSession
}

type p2pRoute struct {
	sessionID  uint32
	controller string
}

func newP2PSessionRegistry() *p2pSessionRegistry {
	return &p2pSessionRegistry{sessions: make(map[uint32]*p2pSession)}
}

func (r *p2pSessionRegistry) register(parent context.Context, msg *protocol.ControlMessage) *p2pSession {
	ctx, cancel := context.WithCancel(parent)
	session := &p2pSession{
		id:         msg.SessionID,
		controller: msg.DeviceID,
		token:      []byte(msg.SessionToken),
		updates:    make(chan []protocol.CandidateInfo, 8),
		ctx:        ctx,
		cancel:     cancel,
	}
	r.mu.Lock()
	if old := r.sessions[msg.SessionID]; old != nil {
		old.cancel()
		close(old.updates)
	}
	r.sessions[msg.SessionID] = session
	r.mu.Unlock()
	return session
}

func (r *p2pSessionRegistry) lookup(sessionID uint64) ([]byte, bool) {
	r.mu.RLock()
	session := r.sessions[uint32(sessionID)]
	r.mu.RUnlock()
	if session == nil || uint64(session.id) != sessionID {
		return nil, false
	}
	select {
	case <-session.ctx.Done():
		return nil, false
	default:
		return append([]byte(nil), session.token...), true
	}
}

func (r *p2pSessionRegistry) update(sessionID uint32, candidates []protocol.CandidateInfo) bool {
	r.mu.RLock()
	session := r.sessions[sessionID]
	if session != nil {
		select {
		case session.updates <- append([]protocol.CandidateInfo(nil), candidates...):
		default:
		}
	}
	r.mu.RUnlock()
	return session != nil
}

func (r *p2pSessionRegistry) remove(sessionID uint32) {
	r.mu.Lock()
	if session := r.sessions[sessionID]; session != nil {
		delete(r.sessions, sessionID)
		session.cancel()
		close(session.updates)
	}
	r.mu.Unlock()
}

func (r *p2pSessionRegistry) close() {
	r.mu.Lock()
	for id, session := range r.sessions {
		delete(r.sessions, id)
		session.cancel()
		close(session.updates)
	}
	r.mu.Unlock()
}

func (r *p2pSessionRegistry) routes() []p2pRoute {
	r.mu.RLock()
	defer r.mu.RUnlock()
	routes := make([]p2pRoute, 0, len(r.sessions))
	for _, session := range r.sessions {
		routes = append(routes, p2pRoute{sessionID: session.id, controller: session.controller})
	}
	return routes
}
