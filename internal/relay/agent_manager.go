package relay

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"rdpulse/internal/acl"
	"rdpulse/internal/config"
	"rdpulse/internal/protocol"
	"rdpulse/internal/ratelimit"
	"rdpulse/internal/signaling"
	"rdpulse/internal/storage"
	"rdpulse/internal/tcp"
	"rdpulse/internal/transport"
	"rdpulse/internal/udp"
)

// AgentManager manages registered agents, their live sessions and control streams
type AgentManager struct {
	mu              sync.RWMutex
	cfg             *config.RelayConfig
	db              *storage.DB
	portManager     *PortManager
	aclManager      *acl.Manager
	coordinator     *signaling.SessionCoordinator
	connectionGuard *ratelimit.IPGuard
	enrollmentGuard *ratelimit.IPGuard
	publicTCPGuard  *ratelimit.IPGuard

	agentsByDevice map[string]*AgentSession
	agentsByPort   map[uint16]*AgentSession
}

const (
	minDeviceSecretBytes    = 32
	maxDeviceSecretBytes    = 4096
	maxEnrollmentTokenBytes = 4096
	maxDeviceIDBytes        = 128
)

// NewAgentManager creates an AgentManager
func NewAgentManager(cfg *config.RelayConfig, db *storage.DB, pm *PortManager, aclMgr *acl.Manager) *AgentManager {
	enrollmentRate := cfg.Security.Limits.MaxEnrollmentRatePerMinute
	if enrollmentRate <= 0 {
		enrollmentRate = 5
	}
	publicTCPConcurrent := cfg.Security.Limits.MaxPublicTCPPerIP
	if publicTCPConcurrent <= 0 {
		publicTCPConcurrent = 32
	}
	publicTCPRate := cfg.Security.Limits.MaxPublicTCPRatePerSecond
	if publicTCPRate <= 0 {
		publicTCPRate = 20
	}
	manager := &AgentManager{
		cfg:             cfg,
		db:              db,
		portManager:     pm,
		aclManager:      aclMgr,
		coordinator:     signaling.NewSessionCoordinator(),
		connectionGuard: ratelimit.NewIPGuard(cfg.Security.Limits.MaxConnectionsPerIP, cfg.Security.Limits.MaxConnectionRatePerMinute, time.Minute),
		enrollmentGuard: ratelimit.NewIPGuard(1, enrollmentRate, time.Minute),
		publicTCPGuard:  ratelimit.NewIPGuard(publicTCPConcurrent, publicTCPRate, time.Second),
		agentsByDevice:  make(map[string]*AgentSession),
		agentsByPort:    make(map[uint16]*AgentSession),
	}
	manager.coordinator.SetAuthorizer(manager.controllerCanAccess)
	if db != nil {
		for _, invitation := range cfg.Security.EnrollmentInvitations {
			if err := db.UpsertEnrollmentInvitation(invitation.DeviceID, invitation.Token, invitation.ExpiresAt); err != nil {
				log.Printf("[Relay] Failed to configure enrollment invitation for %s: %v", invitation.DeviceID, err)
			}
		}
	}
	return manager
}

// Coordinator returns the signaling session coordinator
func (m *AgentManager) Coordinator() *signaling.SessionCoordinator {
	return m.coordinator
}

// OnlineCount returns current online agent count
func (m *AgentManager) OnlineCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.agentsByDevice)
}

// HandleIncomingConnection handles an incoming QUIC Transport connection from an Agent
func (m *AgentManager) HandleIncomingConnection(parentCtx context.Context, tr transport.Transport) {
	defer tr.Close()
	releaseConnection, allowed := m.connectionGuard.Acquire(tr.RemoteAddr())
	if !allowed {
		log.Printf("[Relay] Connection rate/concurrency limit exceeded for %s", tr.RemoteAddr())
		return
	}
	defer releaseConnection()

	// 1. Accept Control Stream
	acceptCtx, cancel := context.WithTimeout(parentCtx, 15*time.Second)
	stream, err := tr.AcceptStream(acceptCtx)
	cancel()
	if err != nil {
		log.Printf("[Relay] Failed to accept control stream from %s: %v", tr.RemoteAddr(), err)
		return
	}
	defer stream.Close()
	controlWriter := protocol.NewControlWriter(stream)
	_ = stream.SetReadDeadline(time.Now().Add(10 * time.Second))

	// 2. Read AUTH message
	authMsg, err := protocol.ReadControlMessage(stream)
	if err != nil {
		log.Printf("[Relay] Failed to read AUTH message: %v", err)
		return
	}
	if authMsg.Type != protocol.MsgTypeAuth {
		_ = controlWriter.WriteMessage(&protocol.ControlMessage{
			Type:    protocol.MsgTypeError,
			Code:    400,
			Message: "expected AUTH message",
		})
		return
	}

	deviceID := authMsg.DeviceID
	if !validDeviceID(deviceID) || authMsg.Secret == "" {
		_ = controlWriter.WriteMessage(&protocol.ControlMessage{
			Type:    protocol.MsgTypeError,
			Code:    400,
			Message: "invalid device_id or missing secret",
		})
		return
	}
	if len(authMsg.Secret) > maxDeviceSecretBytes {
		_ = controlWriter.WriteMessage(&protocol.ControlMessage{
			Type:    protocol.MsgTypeError,
			Code:    400,
			Message: "secret exceeds maximum size",
		})
		return
	}

	// Never accept a client-supplied hash as a credential. That would turn the
	// stored verifier into a reusable pass-the-hash secret.
	if authMsg.SecretHash != "" {
		_ = controlWriter.WriteMessage(&protocol.ControlMessage{
			Type:    protocol.MsgTypeError,
			Code:    400,
			Message: "secret_hash authentication is not supported",
		})
		return
	}

	if m.db == nil {
		log.Printf("[Relay] Authentication unavailable for device %s: database is nil", deviceID)
		_ = controlWriter.WriteMessage(&protocol.ControlMessage{
			Type:    protocol.MsgTypeError,
			Code:    503,
			Message: "authentication service unavailable",
		})
		return
	}

	secretHash := hashSecret(authMsg.Secret)
	record, err := m.db.GetAgentByDeviceID(deviceID)
	if err != nil {
		log.Printf("[Relay] Database error fetching agent %s: %v", deviceID, err)
		_ = controlWriter.WriteMessage(&protocol.ControlMessage{
			Type:    protocol.MsgTypeError,
			Code:    503,
			Message: "authentication service unavailable",
		})
		return
	}

	newDevice := record == nil
	enrollmentToken := authMsg.EnrollmentToken
	isReEnrolling := false
	if record != nil {
		if !record.Enabled {
			log.Printf("[Relay] Authentication rejected for disabled device %s", deviceID)
			_ = controlWriter.WriteMessage(&protocol.ControlMessage{
				Type:    protocol.MsgTypeError,
				Code:    403,
				Message: "device is disabled",
			})
			return
		}
		if subtle.ConstantTimeCompare([]byte(record.SecretHash), []byte(secretHash)) != 1 {
			// Check if client supplied an enrollment token or if an active unconsumed invitation exists
			hasValidInvite := false
			if m.db != nil {
				if enrollmentToken != "" {
					hasValidInvite, _ = m.db.ValidateEnrollmentInvitation(deviceID, enrollmentToken, time.Now())
				} else {
					// Fallback: check if authMsg.Secret itself matches an active invitation
					if valid, _ := m.db.ValidateEnrollmentInvitation(deviceID, authMsg.Secret, time.Now()); valid {
						hasValidInvite = true
						enrollmentToken = authMsg.Secret
					} else {
						invs, _ := m.db.ListEnrollmentInvitations()
						for _, inv := range invs {
							if inv.DeviceID == deviceID && inv.ConsumedAt == nil && inv.ExpiresAt.After(time.Now()) {
								hasValidInvite = true
								break
							}
						}
					}
				}
			}

			if hasValidInvite {
				log.Printf("[Relay] Device %s has active enrollment invitation; proceeding with re-enrollment", deviceID)
				isReEnrolling = true
			} else {
				log.Printf("[Relay] Authentication rejected for device %s", deviceID)
				_ = controlWriter.WriteMessage(&protocol.ControlMessage{
					Type:    protocol.MsgTypeError,
					Code:    401,
					Message: "invalid secret",
				})
				return
			}
		}
	}

	if record == nil || isReEnrolling {
		newDevice = true
		if !m.enrollmentGuard.Allow(tr.RemoteAddr()) {
			_ = controlWriter.WriteMessage(&protocol.ControlMessage{Type: protocol.MsgTypeError, Code: 429, Message: "enrollment attempt rate exceeded"})
			return
		}
		if enrollmentToken == "" {
			// Fallback: check if client's Secret itself matches an active enrollment invitation
			if valid, _ := m.db.ValidateEnrollmentInvitation(deviceID, authMsg.Secret, time.Now()); valid {
				enrollmentToken = authMsg.Secret
			} else {
				_ = controlWriter.WriteMessage(&protocol.ControlMessage{
					Type:    protocol.MsgTypeError,
					Code:    428,
					Message: "device enrollment invitation required",
				})
				retry, readErr := protocol.ReadControlMessage(stream)
				if readErr != nil {
					return
				}
				if retry.Type != protocol.MsgTypeAuth || retry.DeviceID != deviceID || retry.SecretHash != "" ||
					subtle.ConstantTimeCompare([]byte(retry.Secret), []byte(authMsg.Secret)) != 1 {
					_ = controlWriter.WriteMessage(&protocol.ControlMessage{Type: protocol.MsgTypeError, Code: 400, Message: "invalid enrollment retry"})
					return
				}
				enrollmentToken = retry.EnrollmentToken
			}
		}
		if len(enrollmentToken) > maxEnrollmentTokenBytes {
			_ = controlWriter.WriteMessage(&protocol.ControlMessage{Type: protocol.MsgTypeError, Code: 400, Message: "enrollment invitation exceeds maximum size"})
			return
		}
		invitationValid, validationErr := m.db.ValidateEnrollmentInvitation(deviceID, enrollmentToken, time.Now())
		if validationErr != nil {
			log.Printf("[Relay] Enrollment lookup failed for %s: %v", deviceID, validationErr)
			_ = controlWriter.WriteMessage(&protocol.ControlMessage{Type: protocol.MsgTypeError, Code: 503, Message: "enrollment service unavailable"})
			return
		}
		if !invitationValid {
			log.Printf("[Relay] Enrollment rejected for unknown device %s", deviceID)
			_ = controlWriter.WriteMessage(&protocol.ControlMessage{
				Type:    protocol.MsgTypeError,
				Code:    401,
				Message: "enrollment invitation is invalid, expired, or already consumed",
			})
			return
		}
		if len(authMsg.Secret) < minDeviceSecretBytes {
			_ = controlWriter.WriteMessage(&protocol.ControlMessage{
				Type:    protocol.MsgTypeError,
				Code:    400,
				Message: "new device secret must be at least 32 bytes",
			})
			return
		}
	}

	if err := controlWriter.WriteMessage(&protocol.ControlMessage{
		Type: protocol.MsgTypeAuthOK,
	}); err != nil {
		return
	}
	_ = stream.SetReadDeadline(time.Now().Add(10 * time.Second))

	// 3. Read REGISTER or CONNECT_REQUEST message
	initMsg, err := protocol.ReadControlMessage(stream)
	if err != nil {
		log.Printf("[Relay] Failed to read initial message from %s: %v", deviceID, err)
		return
	}
	_ = stream.SetReadDeadline(time.Time{})

	if initMsg.Type == protocol.MsgTypeConnectRequest {
		// Controller Agent session
		if initMsg.DeviceID != "" && initMsg.DeviceID != deviceID {
			_ = controlWriter.WriteMessage(&protocol.ControlMessage{
				Type:    protocol.MsgTypeError,
				Code:    400,
				Message: "device_id does not match authenticated device",
			})
			return
		}
		if !validDeviceID(initMsg.TargetDeviceID) {
			_ = controlWriter.WriteMessage(&protocol.ControlMessage{
				Type:    protocol.MsgTypeError,
				Code:    400,
				Message: "invalid target_device_id",
			})
			return
		}

		if !m.controllerCanAccess(deviceID, initMsg.TargetDeviceID) {
			log.Printf("[Relay] Controller access denied for %s -> %s", deviceID, initMsg.TargetDeviceID)
			_ = controlWriter.WriteMessage(&protocol.ControlMessage{
				Type:    protocol.MsgTypeError,
				Code:    403,
				Message: "controller is not authorized for target",
			})
			return
		}

		if newDevice {
			if err := m.db.CreateAgentWithEnrollment(&storage.AgentRecord{
				DeviceID:   deviceID,
				Hostname:   initMsg.Hostname,
				SecretHash: secretHash,
				PublicPort: 0,
				Enabled:    true,
			}, enrollmentToken, time.Now()); err != nil {
				log.Printf("[Relay] Failed to persist controller %s: %v", deviceID, err)
				_ = controlWriter.WriteMessage(&protocol.ControlMessage{
					Type:    protocol.MsgTypeError,
					Code:    503,
					Message: "failed to enroll controller",
				})
				return
			}
		}

		// Controller connections are session-scoped and must not replace a
		// controlled Agent peer registered under the same device ID.
		controllerPeer := &signaling.ActivePeer{
			DeviceID:      deviceID,
			Hostname:      initMsg.Hostname,
			ControlStream: stream,
			ControlWriter: controlWriter,
			Candidates:    initMsg.Candidates,
		}
		targetSession := m.GetAgentByDeviceID(initMsg.TargetDeviceID)
		if targetSession == nil {
			_ = controlWriter.WriteMessage(&protocol.ControlMessage{
				Type:    protocol.MsgTypeError,
				Code:    404,
				Message: "target agent is offline",
			})
			return
		}

		resp, err := m.coordinator.HandleConnectRequest(controllerPeer, initMsg, m.cfg.RDP.PublicHost)
		if err != nil || resp == nil {
			log.Printf("[Relay] Failed to coordinate connect request for %s -> %s: %v", deviceID, initMsg.TargetDeviceID, err)
			_ = controlWriter.WriteMessage(&protocol.ControlMessage{
				Type:    protocol.MsgTypeError,
				Code:    500,
				Message: "coordination failed",
			})
			return
		}
		if resp.Type == protocol.MsgTypeError {
			_ = controlWriter.WriteMessage(resp)
			return
		}

		controllerCtx, controllerCancel := context.WithCancel(parentCtx)
		defer controllerCancel()
		targetSession.registerControllerRoute(resp.SessionID, tr)
		defer targetSession.unregisterControllerRoute(resp.SessionID, tr)
		defer m.coordinator.CloseSession(resp.SessionID, deviceID)

		if err := controlWriter.WriteMessage(resp); err != nil {
			return
		}

		go m.runControllerStreamLoop(controllerCtx, tr, targetSession)
		if transport.SupportsDatagrams(tr) && transport.SupportsDatagrams(targetSession.Transport) {
			go m.runControllerDatagramLoop(controllerCtx, tr, targetSession, resp.SessionID)
		}

		// Keep controller control loop alive for candidate exchange / pings
		for {
			ctrlMsg, err := protocol.ReadControlMessage(stream)
			if err != nil {
				return
			}
			switch ctrlMsg.Type {
			case protocol.MsgTypePing:
				_ = controlWriter.WriteMessage(&protocol.ControlMessage{
					Type:      protocol.MsgTypePong,
					Timestamp: time.Now().Unix(),
				})
			case protocol.MsgTypeCandidateExchange:
				if ctrlMsg.TargetDeviceID == initMsg.TargetDeviceID &&
					m.controllerCanAccess(deviceID, ctrlMsg.TargetDeviceID) {
					_ = m.coordinator.ForwardCandidateExchange(ctrlMsg.SessionID, deviceID, ctrlMsg.TargetDeviceID, ctrlMsg.Candidates)
				}
			}
		}
	}

	if initMsg.Type != protocol.MsgTypeRegister {
		log.Printf("[Relay] Expected REGISTER or CONNECT_REQUEST, got: %s", initMsg.Type)
		return
	}
	regMsg := initMsg
	if regMsg.DeviceID != "" && regMsg.DeviceID != deviceID {
		_ = controlWriter.WriteMessage(&protocol.ControlMessage{
			Type:    protocol.MsgTypeError,
			Code:    400,
			Message: "device_id does not match authenticated device",
		})
		return
	}

	// 4. Allocate or retrieve assigned public port
	publicPort, err := m.portManager.GetOrAllocatePort(deviceID)
	if err != nil {
		log.Printf("[Relay] Failed to allocate port for device %s: %v", deviceID, err)
		_ = controlWriter.WriteMessage(&protocol.ControlMessage{
			Type:    protocol.MsgTypeError,
			Code:    503,
			Message: err.Error(),
		})
		return
	}
	// The replacement must release the old public listeners before binding the
	// stable port assigned to this device.
	m.closeExistingAgent(deviceID)

	// Existing registrations never rewrite credentials or the administrative
	// enabled flag. Only a newly enrolled device creates those fields.
	var persistErr error
	if newDevice {
		persistErr = m.db.CreateAgentWithEnrollment(&storage.AgentRecord{
			DeviceID:   deviceID,
			Hostname:   regMsg.Hostname,
			SecretHash: secretHash,
			PublicPort: publicPort,
			Enabled:    true,
		}, enrollmentToken, time.Now())
	} else {
		persistErr = m.db.UpdateAgentRegistration(deviceID, regMsg.Hostname, publicPort)
	}
	if persistErr != nil {
		log.Printf("[Relay] Failed to persist agent %s: %v", deviceID, persistErr)
		_ = controlWriter.WriteMessage(&protocol.ControlMessage{
			Type:    protocol.MsgTypeError,
			Code:    503,
			Message: "failed to persist registration",
		})
		return
	}

	// 5. Open TCP and UDP listeners on publicPort
	tcpAddr, _ := net.ResolveTCPAddr("tcp", fmt.Sprintf(":%d", publicPort))
	tcpListener, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		log.Printf("[Relay] Failed to listen TCP on port %d: %v", publicPort, err)
		_ = controlWriter.WriteMessage(&protocol.ControlMessage{
			Type:    protocol.MsgTypeError,
			Code:    500,
			Message: fmt.Sprintf("failed to bind tcp port: %v", err),
		})
		return
	}

	udpAddr, _ := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", publicPort))
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		_ = tcpListener.Close()
		log.Printf("[Relay] Failed to listen UDP on port %d: %v", publicPort, err)
		_ = controlWriter.WriteMessage(&protocol.ControlMessage{
			Type:    protocol.MsgTypeError,
			Code:    500,
			Message: fmt.Sprintf("failed to bind udp port: %v", err),
		})
		return
	}
	_ = udpConn.SetReadBuffer(2 * 1024 * 1024)
	_ = udpConn.SetWriteBuffer(2 * 1024 * 1024)

	sessionCtx, sessionCancel := context.WithCancel(parentCtx)

	agentSession := &AgentSession{
		DeviceID:      deviceID,
		Hostname:      regMsg.Hostname,
		PublicPort:    publicPort,
		Transport:     tr,
		ControlStream: stream,
		ControlWriter: controlWriter,
		TCPListener:   tcpListener,
		UDPListener:   udpConn,
		Sessions: udp.NewSessionManagerWithLimits(m.cfg.UDP.SessionIdleTimeout, nil, udp.SessionLimits{
			MaxSessions:        m.cfg.UDP.MaxSessionsPerAgent,
			MaxSessionsPerIP:   m.cfg.UDP.MaxSessionsPerIP,
			MaxCreatePerSecond: m.cfg.UDP.MaxSessionCreateRate,
		}),
		Reassembler: udp.NewReassembler(m.cfg.UDP.ReassemblyTimeout),
		Fragmenter:  udp.NewFragmenter(m.cfg.UDP.DatagramPayload),
		ACLManager:  m.aclManager,
		TCPGuard:    m.publicTCPGuard,
		ctx:         sessionCtx,
		cancel:      sessionCancel,
	}
	agentSession.LastSeen.Store(time.Now().UnixNano())
	agentSession.Sessions.StartSweeper(sessionCtx, 10*time.Second)
	agentSession.Reassembler.StartCleaner(sessionCtx, 50*time.Millisecond)

	// Close old live session if device was already connected
	m.registerOnlineAgent(agentSession)
	_ = m.coordinator.RegisterPeer(deviceID, regMsg.Hostname, stream, controlWriter, regMsg.Candidates, publicPort)
	defer m.unregisterOnlineAgent(agentSession)

	// 6. Send PORT_ASSIGN
	publicHost := m.cfg.RDP.PublicHost
	if publicHost == "" {
		publicHost = "127.0.0.1"
	}
	_ = controlWriter.WriteMessage(&protocol.ControlMessage{
		Type:       protocol.MsgTypePortAssign,
		PublicHost: publicHost,
		PublicPort: publicPort,
	})

	log.Printf("[Relay] Agent connected: DeviceID=%s, Port=%d, PublicHost=%s", deviceID, publicPort, publicHost)

	// 7. Start TCP and UDP client listeners and Agent datagram loop
	agentSession.StartTCPClientListener()
	if transport.SupportsDatagrams(tr) {
		agentSession.StartUDPClientListener()
		agentSession.StartUDPReceiveFromAgent()
	}

	// 8. Control Stream Event Loop
	m.runControlLoop(agentSession)
}

func (m *AgentManager) closeExistingAgent(deviceID string) {
	m.mu.RLock()
	old := m.agentsByDevice[deviceID]
	m.mu.RUnlock()
	if old != nil {
		old.Close()
	}
}

func (m *AgentManager) runControllerStreamLoop(ctx context.Context, controllerTr transport.Transport, target *AgentSession) {
	const maxConcurrentStreams = 128
	sem := make(chan struct{}, maxConcurrentStreams)
	for {
		stream, err := controllerTr.AcceptStream(ctx)
		if err != nil {
			return
		}

		select {
		case sem <- struct{}{}:
			go func() {
				defer func() { <-sem }()
				m.handleControllerRelayStream(ctx, stream, target)
			}()
		default:
			_ = stream.Close()
		}
	}
}

func (m *AgentManager) handleControllerRelayStream(ctx context.Context, controllerStream transport.Stream, target *AgentSession) {
	defer controllerStream.Close()

	openCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	targetStream, err := target.Transport.OpenStream(openCtx)
	if err != nil {
		return
	}
	defer targetStream.Close()

	reqHdr := &protocol.TCPHeader{
		Version:      protocol.TCPProtocolVersion,
		Type:         protocol.TCPTypeOpen,
		ConnectionID: target.NextConnID(),
		TargetPort:   3389,
	}
	if err := protocol.WriteTCPHeader(targetStream, reqHdr); err != nil {
		return
	}
	respHdr, err := protocol.ReadTCPHeader(targetStream)
	if err != nil || respHdr.Type != protocol.TCPTypeOpenOK {
		return
	}

	errCh := make(chan error, 2)
	go func() {
		_, copyErr := tcp.Copy(targetStream, controllerStream)
		errCh <- copyErr
	}()
	go func() {
		_, copyErr := tcp.Copy(controllerStream, targetStream)
		errCh <- copyErr
	}()

	<-errCh
	controllerStream.CancelRead(0)
	controllerStream.CancelWrite(0)
	targetStream.CancelRead(0)
	targetStream.CancelWrite(0)
	_ = controllerStream.Close()
	_ = targetStream.Close()
	<-errCh
}

func (m *AgentManager) runControllerDatagramLoop(ctx context.Context, controllerTr transport.Transport, target *AgentSession, sessionID uint32) {
	for {
		datagram, err := controllerTr.ReceiveDatagram(ctx)
		if err != nil {
			return
		}
		hdr, _, err := protocol.DecodeUDP(datagram)
		if err != nil || hdr.SessionID != sessionID {
			continue
		}
		if err := target.Transport.SendDatagram(datagram); err != nil {
			return
		}
	}
}

func (m *AgentManager) registerOnlineAgent(sess *AgentSession) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if old, exists := m.agentsByDevice[sess.DeviceID]; exists {
		old.Close()
	}
	m.agentsByDevice[sess.DeviceID] = sess
	m.agentsByPort[sess.PublicPort] = sess
}

func (m *AgentManager) unregisterOnlineAgent(sess *AgentSession) {
	m.coordinator.UnregisterPeer(sess.DeviceID, sess.ControlWriter)

	m.mu.Lock()
	if current, exists := m.agentsByDevice[sess.DeviceID]; exists && current == sess {
		delete(m.agentsByDevice, sess.DeviceID)
		delete(m.agentsByPort, sess.PublicPort)
	}
	m.mu.Unlock()

	sess.Close()
	if m.db != nil {
		_ = m.db.UpdateAgentLastSeen(sess.DeviceID, time.Now())
	}
	log.Printf("[Relay] Agent disconnected: DeviceID=%s, Port=%d", sess.DeviceID, sess.PublicPort)
}

func (m *AgentManager) runControlLoop(sess *AgentSession) {
	for {
		msg, err := protocol.ReadControlMessage(sess.ControlStream)
		if err != nil {
			return
		}

		sess.LastSeen.Store(time.Now().UnixNano())

		switch msg.Type {
		case protocol.MsgTypePing:
			_ = sess.ControlWriter.WriteMessage(&protocol.ControlMessage{
				Type:      protocol.MsgTypePong,
				Timestamp: time.Now().Unix(),
			})
		case protocol.MsgTypeAgentStatus:
			sess.RDPOnline.Store(msg.RDPOnline)
		case protocol.MsgTypeUDPClose:
			sess.Sessions.Remove(msg.SessionID)
		case protocol.MsgTypeConnectRequest:
			_ = sess.ControlWriter.WriteMessage(&protocol.ControlMessage{
				Type:    protocol.MsgTypeError,
				Code:    409,
				Message: "open a dedicated controller connection for CONNECT_REQUEST",
			})
		case protocol.MsgTypeCandidateExchange:
			if m.controllerCanAccess(msg.TargetDeviceID, sess.DeviceID) {
				_ = m.coordinator.ForwardCandidateExchange(msg.SessionID, sess.DeviceID, msg.TargetDeviceID, msg.Candidates)
			}
		}
	}
}

func (m *AgentManager) controllerCanAccess(controllerID, targetID string) bool {
	if controllerID == "" || targetID == "" {
		return false
	}
	if m == nil || m.cfg == nil {
		return false
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.cfg.Security.AllowAllControllerTargets {
		return true
	}

	isAllowed := func(controllers []string) bool {
		for _, allowedController := range controllers {
			if allowedController == "*" || allowedController == controllerID {
				return true
			}
		}
		return false
	}

	return isAllowed(m.cfg.Security.ControllerAccess[targetID]) ||
		isAllowed(m.cfg.Security.ControllerAccess["*"])
}

func validDeviceID(deviceID string) bool {
	if deviceID == "" || len(deviceID) > maxDeviceIDBytes {
		return false
	}
	for i := 0; i < len(deviceID); i++ {
		ch := deviceID[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.' {
			continue
		}
		return false
	}
	return true
}

// GetAgentByDeviceID returns live session
func (m *AgentManager) GetAgentByDeviceID(deviceID string) *AgentSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.agentsByDevice[deviceID]
}

// GetAgentByPort returns live session for port
func (m *AgentManager) GetAgentByPort(port uint16) *AgentSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.agentsByPort[port]
}

// OnlineDeviceInfo provides a snapshot of an active agent session
type OnlineDeviceInfo struct {
	DeviceID   string `json:"deviceId"`
	Hostname   string `json:"hostname"`
	PublicPort uint16 `json:"publicPort"`
	RemoteAddr string `json:"remoteAddr"`
	RDPOnline  bool   `json:"rdpOnline"`
	LastSeen   int64  `json:"lastSeen"`
}

// GetOnlineDevices returns all currently connected devices
func (m *AgentManager) GetOnlineDevices() []OnlineDeviceInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	devices := make([]OnlineDeviceInfo, 0, len(m.agentsByDevice))
	for _, sess := range m.agentsByDevice {
		remote := ""
		if sess.Transport != nil && sess.Transport.RemoteAddr() != nil {
			remote = sess.Transport.RemoteAddr().String()
		}
		devices = append(devices, OnlineDeviceInfo{
			DeviceID:   sess.DeviceID,
			Hostname:   sess.Hostname,
			PublicPort: sess.PublicPort,
			RemoteAddr: remote,
			RDPOnline:  sess.RDPOnline.Load(),
			LastSeen:   sess.LastSeen.Load(),
		})
	}
	return devices
}

// DisconnectDevice disconnects a live agent session by device ID
func (m *AgentManager) DisconnectDevice(deviceID string) bool {
	m.mu.Lock()
	sess, exists := m.agentsByDevice[deviceID]
	if exists {
		delete(m.agentsByDevice, deviceID)
		delete(m.agentsByPort, sess.PublicPort)
	}
	m.mu.Unlock()
	if exists {
		sess.Close()
		return true
	}
	return false
}

// UpdateAccessPolicy dynamically updates the controller access policy
func (m *AgentManager) UpdateAccessPolicy(allowAll bool, accessMap map[string][]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.Security.AllowAllControllerTargets = allowAll
	if accessMap != nil {
		m.cfg.Security.ControllerAccess = accessMap
	}
}

// GetAccessPolicy returns the current controller access policy
func (m *AgentManager) GetAccessPolicy() (bool, map[string][]string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	copyMap := make(map[string][]string, len(m.cfg.Security.ControllerAccess))
	for k, v := range m.cfg.Security.ControllerAccess {
		vals := make([]string, len(v))
		copy(vals, v)
		copyMap[k] = vals
	}
	return m.cfg.Security.AllowAllControllerTargets, copyMap
}

// ReleaseDevicePort releases the public port allocation for a deleted device
func (m *AgentManager) ReleaseDevicePort(deviceID string) {
	if m.portManager != nil {
		m.portManager.ReleasePort(deviceID)
	}
}

// Close closes all agent sessions
func (m *AgentManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sess := range m.agentsByDevice {
		sess.Close()
	}
	m.agentsByDevice = make(map[string]*AgentSession)
	m.agentsByPort = make(map[uint16]*AgentSession)
	return nil
}
