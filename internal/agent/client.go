package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"rdpulse/internal/config"
	"rdpulse/internal/nat"
	"rdpulse/internal/protocol"
	"rdpulse/internal/punch"
	"rdpulse/internal/tcp"
	"rdpulse/internal/transport"
	"rdpulse/internal/udp"
)

// Client represents the Windows Agent client connected to Relay
type Client struct {
	cfg              *config.AgentConfig
	backoff          *Backoff
	nextPacketID     atomic.Uint32
	OnInitialConnect func(err error)
	initOnce         sync.Once
}

func (c *Client) notifyInitialConnect(err error) {
	c.initOnce.Do(func() {
		if c.OnInitialConnect != nil {
			c.OnInitialConnect(err)
		}
	})
}

// NewClient creates an Agent client
func NewClient(cfg *config.AgentConfig) *Client {
	return &Client{
		cfg:     cfg,
		backoff: NewBackoff(1*time.Second, cfg.Reconnect.MaxInterval),
	}
}

// NextPacketID returns incremented datagram packet ID
func (c *Client) NextPacketID() uint32 {
	return c.nextPacketID.Add(1)
}

// Run starts the agent connection loop with exponential backoff and jitter
func (c *Client) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := c.connectAndServe(ctx)
		c.notifyInitialConnect(err)
		if err != nil && !errors.Is(err, context.Canceled) {
			delay := c.backoff.NextDelay()
			log.Printf("[Agent] Connection lost (%v), reconnecting in %v...", err, delay)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		} else {
			c.backoff.Reset()
		}
	}
}

func (c *Client) buildTLSConfig() (*tls.Config, error) {
	tlsConf := &tls.Config{
		NextProtos: []string{"rdpulse-quic"},
	}

	if c.cfg.Server.InsecureSkipVerify {
		tlsConf.InsecureSkipVerify = true
	} else if c.cfg.Server.CACert != "" {
		caData, err := os.ReadFile(c.cfg.Server.CACert)
		if err != nil {
			return nil, fmt.Errorf("read ca cert failed: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caData) {
			return nil, errors.New("failed to parse ca certificate")
		}
		tlsConf.RootCAs = pool
	}

	return tlsConf, nil
}

func (c *Client) connectAndServe(parentCtx context.Context) error {
	connCtx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	log.Printf("[Agent] Connecting to Relay at %s...", c.cfg.Server.Address)
	tr, transportName, err := c.dialRelayTransport(connCtx)
	if err != nil {
		return fmt.Errorf("dial relay failed: %w", err)
	}
	defer tr.Close()
	log.Printf("[Agent] Connected using %s transport", transportName)

	// 1. Open Control Stream
	controlStream, err := tr.OpenStream(connCtx)
	if err != nil {
		return fmt.Errorf("open control stream failed: %w", err)
	}
	defer controlStream.Close()
	controlWriter := protocol.NewControlWriter(controlStream)

	// 2. Authenticate. Enrollment credentials are sent only after an explicit
	// unknown-device challenge.
	if err := c.authenticateControl(controlStream, controlWriter); err != nil {
		return err
	}

	// 3. Open the shared UDP dispatcher socket and authenticated direct TCP listener.
	punchUDPConn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return fmt.Errorf("listen p2p punch socket failed: %w", err)
	}
	tuneUDPSocketBuffers(punchUDPConn)
	p2pPort := punchUDPConn.LocalAddr().(*net.UDPAddr).Port
	directTCPListener, err := net.ListenTCP("tcp", &net.TCPAddr{})
	if err != nil {
		return fmt.Errorf("listen direct tcp socket failed: %w", err)
	}
	defer directTCPListener.Close()
	directTCPPort := directTCPListener.Addr().(*net.TCPAddr).Port
	p2pSessions := newP2PSessionRegistry()
	defer p2pSessions.close()

	hostname, _ := os.Hostname()
	localCandidates := c.discoverP2PCandidates(connCtx, punchUDPConn, p2pPort, directTCPPort)
	dispatcher := punch.NewUDPDispatcher(punchUDPConn)
	defer dispatcher.Close()
	regMsg := &protocol.ControlMessage{
		Type:               protocol.MsgTypeRegister,
		DeviceID:           c.cfg.Device.ID,
		Hostname:           hostname,
		AgentVersion:       "2.0.0",
		MinProtocolVersion: 1,
		MaxProtocolVersion: 2,
		Candidates:         localCandidates,
	}
	if err := controlWriter.WriteMessage(regMsg); err != nil {
		return fmt.Errorf("send register failed: %w", err)
	}

	// 4. Read PORT_ASSIGN
	assignResp, err := protocol.ReadControlMessage(controlStream)
	if err != nil {
		return fmt.Errorf("read port assignment failed: %w", err)
	}
	if assignResp.Type != protocol.MsgTypePortAssign {
		return fmt.Errorf("expected PORT_ASSIGN, got: %s", assignResp.Type)
	}

	log.Printf("[Agent] Connected! RDP endpoint: %s:%d", assignResp.PublicHost, assignResp.PublicPort)
	c.notifyInitialConnect(nil)

	// Successfully connected, reset backoff
	c.backoff.Reset()

	// Setup managers
	sessionManager := udp.NewSessionManager(c.cfg.UDP.SessionIdleTimeout, nil)
	defer sessionManager.Close()
	sessionManager.StartSweeper(connCtx, 10*time.Second)

	reassembler := udp.NewReassembler(c.cfg.UDP.ReassemblyTimeout)
	reassembler.StartCleaner(connCtx, 50*time.Millisecond)

	fragmenter := udp.NewFragmenter(c.cfg.Transport.DatagramPayload)

	errCh := make(chan error, 6)

	// 5. Run incoming QUIC Streams loop (TCP Forwarding)
	go func() {
		errCh <- c.acceptStreamsLoop(connCtx, tr)
	}()

	// 6. QUIC supports native unreliable Datagram relay. TLS/TCP fallback
	// intentionally disables RDP UDP to avoid head-of-line blocking.
	if transport.SupportsDatagrams(tr) {
		go func() {
			errCh <- c.receiveDatagramsLoop(connCtx, tr, sessionManager, reassembler, fragmenter)
		}()
	} else {
		log.Printf("[Agent] %s transport active; RDP UDP relay is disabled", transportName)
	}

	// 7. Run Heartbeat loop
	go func() {
		errCh <- c.heartbeatLoop(connCtx, controlWriter)
	}()

	// 8. Run Control Messages Reader loop (Signaling & P2P Punch)
	go func() {
		errCh <- c.controlMessageReader(connCtx, controlStream, sessionManager, reassembler, fragmenter, dispatcher, p2pSessions)
	}()
	if !c.cfg.Transport.DisableP2P {
		go c.refreshControlledCandidates(connCtx, controlWriter, p2pSessions, p2pPort, directTCPPort, localCandidates)
	}

	// 9. Accept one independently authenticated direct TCP connection for each
	// local RDP TCP connection opened by a Controller.
	go func() {
		errCh <- c.acceptDirectTCP(connCtx, directTCPListener, p2pSessions)
	}()

	// 10. Run RDP Health Check loop
	healthChecker := NewHealthChecker(c.cfg.RDP.Address, 10*time.Second, func(online bool) {
		_ = controlWriter.WriteMessage(&protocol.ControlMessage{
			Type:      protocol.MsgTypeAgentStatus,
			RDPOnline: online,
		})
	})
	healthChecker.Start(connCtx)

	// Block until any loop exits
	err = <-errCh
	return err
}

func (c *Client) acceptStreamsLoop(ctx context.Context, tr transport.Transport) error {
	for {
		stream, err := tr.AcceptStream(ctx)
		if err != nil {
			return err
		}

		go c.handleIncomingStream(ctx, stream)
	}
}

func (c *Client) handleIncomingStream(ctx context.Context, stream transport.Stream) {
	defer stream.Close()

	// 1. Read TCP header from Relay
	hdr, err := protocol.ReadTCPHeader(stream)
	if err != nil {
		log.Printf("[Agent] Failed to read TCP header from stream: %v", err)
		return
	}

	if hdr.Type != protocol.TCPTypeOpen {
		return
	}

	// 2. Dial local RDP service
	targetAddr := c.cfg.RDP.Address
	if targetAddr == "" {
		targetAddr = "127.0.0.1:3389"
	}

	localConn, err := net.DialTimeout("tcp", targetAddr, 3*time.Second)
	if err != nil {
		log.Printf("[Agent] Failed to dial local RDP %s: %v", targetAddr, err)
		_ = protocol.WriteTCPHeader(stream, &protocol.TCPHeader{
			Version:      protocol.TCPProtocolVersion,
			Type:         protocol.TCPTypeError,
			ConnectionID: hdr.ConnectionID,
		})
		return
	}
	defer localConn.Close()

	// 3. Respond with TCP_OPEN_OK
	respHdr := &protocol.TCPHeader{
		Version:      protocol.TCPProtocolVersion,
		Type:         protocol.TCPTypeOpenOK,
		ConnectionID: hdr.ConnectionID,
		TargetPort:   hdr.TargetPort,
	}
	if err := protocol.WriteTCPHeader(stream, respHdr); err != nil {
		return
	}

	// 4. Proxy data bidirectionally
	p := tcp.NewProxy(localConn, stream, nil)
	_ = p.Run()
}

func (c *Client) receiveDatagramsLoop(
	ctx context.Context,
	tr transport.Transport,
	sm *udp.SessionManager,
	reassembler *udp.Reassembler,
	fragmenter *udp.Fragmenter,
) error {
	rdpUDPAddr, err := net.ResolveUDPAddr("udp", c.cfg.RDP.Address)
	if err != nil {
		return fmt.Errorf("resolve rdp udp address failed: %w", err)
	}

	// assembleBuf is reused across packets; assembled is written to the local
	// RDP socket before the next iteration overwrites it.
	assembleBuf := make([]byte, 0, protocol.MaxUDPPacketSize)

	for {
		datagram, err := tr.ReceiveDatagram(ctx)
		if err != nil {
			return err
		}

		hdr, payload, err := protocol.DecodeUDP(datagram)
		if err != nil {
			continue
		}

		// Reassemble fragment
		assembled, err := reassembler.FeedAppend(assembleBuf, &hdr, payload)
		if err != nil || assembled == nil {
			continue
		}

		// Lookup or implicitly create local UDP session
		sess := sm.GetByID(hdr.SessionID)
		if sess == nil {
			// Create dedicated local UDP socket bound to an ephemeral port
			localConn, err := net.DialUDP("udp", nil, rdpUDPAddr)
			if err != nil {
				log.Printf("[Agent] Failed to create local UDP socket to RDP: %v", err)
				continue
			}

			sess = sm.AddSessionWithID(hdr.SessionID, localConn)
			if sess == nil {
				_ = localConn.Close()
				continue
			}

			// Launch dedicated receive loop for this session's socket
			go c.readFromLocalUDPLoop(ctx, sess, tr, fragmenter)
		}

		// Send packet to local RDP
		if sess.Conn != nil {
			_, _ = sess.Conn.Write(assembled)
		}
	}
}

func (c *Client) readFromLocalUDPLoop(
	ctx context.Context,
	sess *udp.UDPSession,
	tr transport.Transport,
	fragmenter *udp.Fragmenter,
) {
	buf := make([]byte, protocol.MaxUDPPacketSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := sess.Conn.Read(buf)
		if err != nil {
			return
		}

		sess.UpdateActivity()
		packetID := c.NextPacketID()

		// Fragment and send datagrams back to Relay
		_ = fragmenter.Fragment(sess.ID, packetID, buf[:n], func(datagram []byte) error {
			return tr.SendDatagram(datagram)
		})
	}
}

func (c *Client) heartbeatLoop(ctx context.Context, writer *protocol.ControlWriter) error {
	interval := c.cfg.Heartbeat.Interval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			ping := &protocol.ControlMessage{
				Type:      protocol.MsgTypePing,
				Timestamp: time.Now().Unix(),
			}
			if err := writer.WriteMessage(ping); err != nil {
				return err
			}
		}
	}
}

func (c *Client) controlMessageReader(
	ctx context.Context,
	stream transport.Stream,
	sm *udp.SessionManager,
	reassembler *udp.Reassembler,
	fragmenter *udp.Fragmenter,
	dispatcher *punch.UDPDispatcher,
	p2pSessions *p2pSessionRegistry,
) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msg, err := protocol.ReadControlMessage(stream)
		if err != nil {
			return err
		}

		switch msg.Type {
		case protocol.MsgTypeConnectNotify:
			log.Printf("[Agent] Received connect request from Controller %s (SessionID=%d), starting P2P punch...", msg.DeviceID, msg.SessionID)
			session := p2pSessions.register(ctx, msg)
			go c.handleIncomingP2PPunch(session, msg, sm, reassembler, fragmenter, dispatcher)
		case protocol.MsgTypeCandidateExchange:
			_ = p2pSessions.update(msg.SessionID, msg.Candidates)
		case protocol.MsgTypeSessionClose:
			p2pSessions.remove(msg.SessionID)
		}
	}
}

func (c *Client) discoverP2PCandidates(ctx context.Context, punchConn *net.UDPConn, udpPort, tcpPort int) []protocol.CandidateInfo {
	if c.cfg.Transport.DisableP2P {
		return nil
	}
	candidates, err := nat.DiscoverLANCandidates(udpPort, tcpPort)
	if err != nil {
		log.Printf("[Agent] Failed to discover LAN candidates: %v", err)
	}

	if c.cfg.Server.RendezvousAddress != "" {
		candidate, err := nat.ProbeReflexiveCandidate(ctx, c.cfg.Server.RendezvousAddress, punchConn)
		if err != nil {
			log.Printf("[Agent] Failed to discover reflexive UDP candidate: %v", err)
		} else if candidate != nil {
			candidates = append(candidates, *candidate)
		}
	}
	return candidates
}

func (c *Client) handleIncomingP2PPunch(
	session *p2pSession,
	notify *protocol.ControlMessage,
	sm *udp.SessionManager,
	reassembler *udp.Reassembler,
	fragmenter *udp.Fragmenter,
	dispatcher *punch.UDPDispatcher,
) {
	sessionID := uint64(notify.SessionID)
	sessionToken := []byte(notify.SessionToken)

	res, err := punch.PunchUDPDispatched(session.ctx, dispatcher, notify.Candidates, session.updates, sessionID, sessionToken, 3*time.Second)
	if err != nil {
		log.Printf("[Agent] P2P UDP punch with controller %s failed (%v), awaiting Relay fallback", notify.DeviceID, err)
		return
	}

	sessionCtx, sessionCancel := context.WithCancel(session.ctx)
	defer sessionCancel()
	defer res.StopKeepalive()
	defer res.Close()

	log.Printf("[Agent] P2P UDP punch with controller %s SUCCESS! Remote=%s", notify.DeviceID, res.RemoteAddr)
	res.StartKeepalive(sessionCtx, p2pUDPKeepaliveInt)

	// Dial local RDP UDP
	rdpUDPAddr, err := net.ResolveUDPAddr("udp", c.cfg.RDP.Address)
	if err != nil {
		log.Printf("[Agent] Resolve local RDP UDP failed: %v", err)
		return
	}

	localConn, err := net.DialUDP("udp", nil, rdpUDPAddr)
	if err != nil {
		log.Printf("[Agent] Dial local RDP UDP socket failed: %v", err)
		return
	}
	defer localConn.Close()
	tuneUDPSocketBuffers(localConn)

	// The session codec caches the expanded HMAC key so neither direction
	// repeats key setup per datagram.
	codec, err := protocol.NewP2PCodec(notify.SessionID, sessionToken)
	if err != nil {
		log.Printf("[Agent] P2P session codec unavailable: %v", err)
		return
	}

	// 1. From P2P -> Local RDP
	go func() {
		buf := make([]byte, protocol.MaxUDPPacketSize+protocol.P2PDataHeaderSize)
		assembleBuf := make([]byte, 0, protocol.MaxUDPPacketSize)
		timeoutCount := 0
		idleLimit := p2pUDPIdleLimit()
		for {
			select {
			case <-sessionCtx.Done():
				return
			default:
			}

			_ = res.SetReadDeadline(time.Now().Add(p2pUDPReadSlice))
			n, remoteAddr, err := res.ReadFromUDPAddrPort(buf)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					timeoutCount++
					if timeoutCount >= idleLimit {
						log.Printf("[Agent] P2P connection to %s timed out, disconnecting session", notify.DeviceID)
						sessionCancel()
						return
					}
				}
				continue
			}
			if remoteAddr.Port() != res.RemoteAddr.Port() ||
				remoteAddr.Addr().Unmap() != res.RemoteAddr.Addr().Unmap() {
				continue
			}

			timeoutCount = 0
			// Skip punch / keepalive
			if p2pUDPResetsIdle(n, buf) {
				continue
			}

			datagram, err := codec.Decode(buf[:n])
			if err != nil {
				continue
			}
			timeoutCount = 0

			hdr, payload, err := protocol.DecodeUDP(datagram)
			if err != nil {
				continue
			}
			if session.replay.Seen(hdr.PacketID) {
				continue
			}

			assembled, err := reassembler.FeedAppend(assembleBuf, &hdr, payload)
			if err != nil || assembled == nil {
				continue
			}
			if !session.replay.MarkCompleted(hdr.PacketID) {
				continue
			}

			_, _ = localConn.Write(assembled)
		}
	}()

	// 2. From Local RDP -> P2P
	targetAddr := res.RemoteAddr
	rdpBuf := make([]byte, protocol.MaxUDPPacketSize)
	sendBuf := make([]byte, 0, udp.DefaultBufferSize)
	for {
		select {
		case <-sessionCtx.Done():
			return
		default:
		}

		_ = localConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := localConn.Read(rdpBuf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return
		}

		packetID := c.NextPacketID()
		_ = fragmenter.Fragment(notify.SessionID, packetID, rdpBuf[:n], func(datagram []byte) error {
			packet := codec.Encode(sendBuf[:0], datagram)
			_, writeErr := res.Conn.WriteToUDPAddrPort(packet, targetAddr)
			return writeErr
		})
	}
}
