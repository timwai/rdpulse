package signaling

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"rdpulse/internal/protocol"
	"rdpulse/internal/transport"
)

var (
	ErrPeerOffline     = errors.New("target peer is currently offline")
	ErrSessionNotFound = errors.New("session not found")
)

const (
	MaxCandidateCount        = 16
	MaxActiveSessions        = 2048
	MaxActiveSessionsPerPeer = 64
)

// ActivePeer represents a connected agent or controller registered on the signaling server
type ActivePeer struct {
	DeviceID      string
	Hostname      string
	ControlStream transport.Stream
	ControlWriter *protocol.ControlWriter
	Candidates    []protocol.CandidateInfo
	PublicPort    uint16
	LastSeen      atomic.Int64
}

// SessionCoordinator manages P2P signaling, candidate exchange and session tokens
type SessionCoordinator struct {
	mu            sync.RWMutex
	peers         map[string]*ActivePeer
	sessions      map[uint32]*activeSession
	nextSessionID atomic.Uint32
	authorize     func(controllerID, targetID string) bool
}

type activeSession struct {
	id               uint32
	controllerID     string
	targetID         string
	controllerWriter *protocol.ControlWriter
	targetWriter     *protocol.ControlWriter
	createdAt        time.Time
}

// SetAuthorizer installs the mandatory Controller-to-Target authorization
// policy. Without one, connection requests are denied.
func (c *SessionCoordinator) SetAuthorizer(authorize func(controllerID, targetID string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authorize = authorize
}

// NewSessionCoordinator creates a SessionCoordinator
func NewSessionCoordinator() *SessionCoordinator {
	c := &SessionCoordinator{
		peers:    make(map[string]*ActivePeer),
		sessions: make(map[uint32]*activeSession),
	}
	c.nextSessionID.Store(uint32(time.Now().UnixNano()) & 0x7fffffff)
	return c
}

// RegisterPeer registers an active peer control stream and candidates
func (c *SessionCoordinator) RegisterPeer(deviceID, hostname string, stream transport.Stream, writer *protocol.ControlWriter, candidates []protocol.CandidateInfo, publicPort uint16) *ActivePeer {
	c.mu.Lock()
	defer c.mu.Unlock()

	validCandidates, _ := validateCandidates(candidates)
	peer := &ActivePeer{
		DeviceID:      deviceID,
		Hostname:      hostname,
		ControlStream: stream,
		ControlWriter: writer,
		Candidates:    validCandidates,
		PublicPort:    publicPort,
	}
	peer.LastSeen.Store(time.Now().UnixNano())
	c.peers[deviceID] = peer
	return peer
}

// UpdateCandidates updates candidate list for a registered peer
func (c *SessionCoordinator) UpdateCandidates(deviceID string, candidates []protocol.CandidateInfo) {
	validCandidates, err := validateCandidates(candidates)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if peer, exists := c.peers[deviceID]; exists {
		peer.Candidates = validCandidates
	}
}

// UnregisterPeer removes a peer only when the disconnecting session still owns
// the device entry. This prevents an old connection from deleting a newer one.
func (c *SessionCoordinator) UnregisterPeer(deviceID string, writer *protocol.ControlWriter) {
	c.mu.Lock()
	if peer := c.peers[deviceID]; peer != nil && peer.ControlWriter == writer {
		delete(c.peers, deviceID)
	}
	var notifications []*protocol.ControlWriter
	var ids []uint32
	for id, session := range c.sessions {
		if session.targetID == deviceID && session.targetWriter == writer {
			delete(c.sessions, id)
			notifications = append(notifications, session.controllerWriter)
			ids = append(ids, id)
		}
	}
	c.mu.Unlock()
	for i, target := range notifications {
		if target != nil {
			_ = target.WriteMessage(&protocol.ControlMessage{Type: protocol.MsgTypeSessionClose, SessionID: ids[i]})
		}
	}
}

// GetPeer retrieves a registered peer
func (c *SessionCoordinator) GetPeer(deviceID string) *ActivePeer {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.peers[deviceID]
}

// HandleConnectRequest processes a connection initiation from Controller to Target
func (c *SessionCoordinator) HandleConnectRequest(controller *ActivePeer, req *protocol.ControlMessage, publicHost string) (*protocol.ControlMessage, error) {
	if controller == nil || req == nil {
		return nil, errors.New("controller and request are required")
	}
	targetID := req.TargetDeviceID
	if targetID == "" {
		return nil, errors.New("missing target_device_id")
	}
	controllerCandidates, err := validateCandidates(req.Candidates)
	if err != nil {
		return &protocol.ControlMessage{
			Type:    protocol.MsgTypeError,
			Code:    400,
			Message: err.Error(),
		}, nil
	}

	c.mu.RLock()
	authorize := c.authorize
	targetPeer, online := c.peers[targetID]
	var targetCandidates []protocol.CandidateInfo
	var targetPublicPort uint16
	var targetWriter *protocol.ControlWriter
	if targetPeer != nil {
		targetCandidates = append([]protocol.CandidateInfo(nil), targetPeer.Candidates...)
		targetPublicPort = targetPeer.PublicPort
		targetWriter = targetPeer.ControlWriter
	}
	c.mu.RUnlock()
	if authorize == nil || !authorize(controller.DeviceID, targetID) {
		return &protocol.ControlMessage{
			Type:    protocol.MsgTypeError,
			Code:    403,
			Message: "controller is not authorized for target",
		}, nil
	}

	if !online || targetPeer == nil || targetWriter == nil {
		return &protocol.ControlMessage{
			Type:    protocol.MsgTypeError,
			Code:    404,
			Message: ErrPeerOffline.Error(),
		}, nil
	}

	// Generate unique 64-bit session ID and 128-bit random token
	// Reserve the high-bit namespace for authenticated Controller sessions.
	// Compatibility public UDP sessions always clear this bit.
	sessionID := c.nextSessionID.Add(1) | 0x80000000
	var tokenBytes [16]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return nil, fmt.Errorf("generate session token: %w", err)
	}
	sessionToken := hex.EncodeToString(tokenBytes[:])
	c.mu.Lock()
	if len(c.sessions) >= MaxActiveSessions || c.sessionCountLocked(controller.DeviceID) >= MaxActiveSessionsPerPeer || c.sessionCountLocked(targetID) >= MaxActiveSessionsPerPeer {
		c.mu.Unlock()
		return &protocol.ControlMessage{Type: protocol.MsgTypeError, Code: 429, Message: "active signaling session limit exceeded"}, nil
	}
	c.sessions[sessionID] = &activeSession{
		id:               sessionID,
		controllerID:     controller.DeviceID,
		targetID:         targetID,
		controllerWriter: controller.ControlWriter,
		targetWriter:     targetWriter,
		createdAt:        time.Now(),
	}
	c.mu.Unlock()

	// 1. Notify Target Controlled Agent
	targetNotify := &protocol.ControlMessage{
		Type:           protocol.MsgTypeConnectNotify,
		DeviceID:       controller.DeviceID,
		TargetDeviceID: targetID,
		SessionID:      sessionID,
		SessionToken:   sessionToken,
		Candidates:     controllerCandidates,
		PunchRole:      "controlled",
	}

	if err := targetWriter.WriteMessage(targetNotify); err != nil {
		c.CloseSession(sessionID, controller.DeviceID)
		log.Printf("[Signaling] Failed to send connect notify to target %s: %v", targetID, err)
		return nil, fmt.Errorf("notify target failed: %w", err)
	}

	// 2. Respond to Controller Agent
	resp := &protocol.ControlMessage{
		Type:           protocol.MsgTypeConnectResponse,
		DeviceID:       controller.DeviceID,
		TargetDeviceID: targetID,
		SessionID:      sessionID,
		SessionToken:   sessionToken,
		Candidates:     targetCandidates,
		PublicHost:     publicHost,
		PublicPort:     targetPublicPort,
		PunchRole:      "controller",
	}

	return resp, nil
}

func (c *SessionCoordinator) sessionCountLocked(deviceID string) int {
	count := 0
	for _, session := range c.sessions {
		if session.controllerID == deviceID || session.targetID == deviceID {
			count++
		}
	}
	return count
}

// ForwardCandidateExchange forwards peer candidates to counterpart
func (c *SessionCoordinator) ForwardCandidateExchange(sessionID uint32, fromDeviceID, toDeviceID string, candidates []protocol.CandidateInfo) error {
	validCandidates, err := validateCandidates(candidates)
	if err != nil {
		return err
	}
	c.mu.RLock()
	session := c.sessions[sessionID]
	authorize := c.authorize
	var targetWriter *protocol.ControlWriter
	var expectedTarget string
	if session != nil {
		switch fromDeviceID {
		case session.controllerID:
			expectedTarget = session.targetID
			targetWriter = session.targetWriter
		case session.targetID:
			expectedTarget = session.controllerID
			targetWriter = session.controllerWriter
		}
	}
	c.mu.RUnlock()

	if session == nil || targetWriter == nil || expectedTarget == "" {
		return ErrSessionNotFound
	}
	if toDeviceID != "" && toDeviceID != expectedTarget {
		return errors.New("candidate exchange target does not match session")
	}
	if authorize == nil || !authorize(session.controllerID, session.targetID) {
		return errors.New("candidate exchange is not authorized")
	}

	msg := &protocol.ControlMessage{
		Type:           protocol.MsgTypeCandidateExchange,
		DeviceID:       fromDeviceID,
		TargetDeviceID: expectedTarget,
		SessionID:      sessionID,
		Candidates:     validCandidates,
	}

	return targetWriter.WriteMessage(msg)
}

// CloseSession removes a session-scoped signaling route and notifies the
// controlled peer so direct TCP credentials and UDP goroutines are revoked.
func (c *SessionCoordinator) CloseSession(sessionID uint32, controllerID string) {
	c.mu.Lock()
	session := c.sessions[sessionID]
	if session == nil || (controllerID != "" && session.controllerID != controllerID) {
		c.mu.Unlock()
		return
	}
	delete(c.sessions, sessionID)
	targetWriter := session.targetWriter
	c.mu.Unlock()
	if targetWriter != nil {
		_ = targetWriter.WriteMessage(&protocol.ControlMessage{
			Type:           protocol.MsgTypeSessionClose,
			DeviceID:       session.controllerID,
			TargetDeviceID: session.targetID,
			SessionID:      sessionID,
		})
	}
}

func validateCandidates(candidates []protocol.CandidateInfo) ([]protocol.CandidateInfo, error) {
	if len(candidates) > MaxCandidateCount {
		return nil, fmt.Errorf("candidate count exceeds limit %d", MaxCandidateCount)
	}
	validated := make([]protocol.CandidateInfo, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate.Protocol != "udp" && candidate.Protocol != "tcp" {
			return nil, errors.New("candidate protocol must be udp or tcp")
		}
		if candidate.Type != "lan" && candidate.Type != "reflexive" {
			return nil, errors.New("candidate type must be lan or reflexive")
		}
		addr, err := netip.ParseAddrPort(candidate.Address)
		if err != nil || !addr.Addr().IsValid() || addr.Addr().IsUnspecified() || addr.Addr().IsMulticast() || addr.Port() == 0 {
			return nil, errors.New("candidate address must be a unicast IP and non-zero port")
		}
		key := candidate.Protocol + "|" + addr.String()
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		candidate.Address = addr.String()
		validated = append(validated, candidate)
	}
	return validated, nil
}

// GenerateSessionID returns an incrementing uint64
func (c *SessionCoordinator) GenerateSessionID() uint64 {
	return uint64(c.nextSessionID.Add(1) | 0x80000000)
}
