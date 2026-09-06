package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"rdpulse/internal/acl"
	"rdpulse/internal/protocol"
	"rdpulse/internal/ratelimit"
	"rdpulse/internal/tcp"
	"rdpulse/internal/transport"
	"rdpulse/internal/udp"
)

// AgentSession encapsulates the state of a connected and authenticated Windows Agent
type AgentSession struct {
	DeviceID      string
	Hostname      string
	PublicPort    uint16
	Transport     transport.Transport
	ControlStream transport.Stream
	ControlWriter *protocol.ControlWriter

	TCPListener *net.TCPListener
	UDPListener *net.UDPConn

	Sessions    *udp.SessionManager
	Reassembler *udp.Reassembler
	Fragmenter  *udp.Fragmenter

	ACLManager *acl.Manager
	TCPGuard   *ratelimit.IPGuard

	RDPOnline atomic.Bool
	LastSeen  atomic.Int64

	nextConnID   atomic.Uint32
	nextPacketID atomic.Uint32

	cancel context.CancelFunc
	ctx    context.Context
	closed atomic.Bool
	mu     sync.Mutex

	routesMu         sync.RWMutex
	controllerRoutes map[uint32]transport.Transport
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// NextPacketID returns an atomically incrementing packet ID for UDP datagrams
func (a *AgentSession) NextPacketID() uint32 {
	return a.nextPacketID.Add(1)
}

// NextConnID returns an atomically incrementing connection ID for TCP streams
func (a *AgentSession) NextConnID() uint32 {
	return a.nextConnID.Add(1)
}

// Close terminates all listeners, datagram loops, and transport
func (a *AgentSession) Close() {
	if a.closed.Swap(true) {
		return
	}
	if a.cancel != nil {
		a.cancel()
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.TCPListener != nil {
		_ = a.TCPListener.Close()
	}
	if a.UDPListener != nil {
		_ = a.UDPListener.Close()
	}
	if a.Sessions != nil {
		_ = a.Sessions.Close()
	}
	if a.ControlStream != nil {
		_ = a.ControlStream.Close()
	}
	if a.Transport != nil {
		_ = a.Transport.Close()
	}
}

// StartUDPReceiveFromAgent listens for QUIC Datagrams coming from Agent and sends to client
func (a *AgentSession) StartUDPReceiveFromAgent() {
	go func() {
		assembleBuf := make([]byte, 0, protocol.MaxUDPPacketSize)
		for {
			select {
			case <-a.ctx.Done():
				return
			default:
			}

			datagram, err := a.Transport.ReceiveDatagram(a.ctx)
			if err != nil {
				return
			}

			hdr, payload, err := protocol.DecodeUDP(datagram)
			if err != nil {
				continue
			}

			// Assemble fragments
			if controllerTr := a.controllerRoute(hdr.SessionID); controllerTr != nil {
				_ = controllerTr.SendDatagram(datagram)
				continue
			}

			// Assemble fragments for compatibility public UDP clients.
			assembled, err := a.Reassembler.FeedAppend(assembleBuf, &hdr, payload)
			if err != nil || assembled == nil {
				continue
			}

			// Find session to determine client return address
			sess := a.Sessions.GetByID(hdr.SessionID)
			if sess == nil {
				continue
			}

			// Forward UDP packet back to mstsc from the same public port
			_, _ = a.UDPListener.WriteToUDPAddrPort(assembled, sess.ClientAddr)
		}
	}()
}

func (a *AgentSession) registerControllerRoute(sessionID uint32, tr transport.Transport) {
	a.routesMu.Lock()
	defer a.routesMu.Unlock()
	if a.controllerRoutes == nil {
		a.controllerRoutes = make(map[uint32]transport.Transport)
	}
	a.controllerRoutes[sessionID] = tr
}

func (a *AgentSession) unregisterControllerRoute(sessionID uint32, tr transport.Transport) {
	a.routesMu.Lock()
	defer a.routesMu.Unlock()
	if current := a.controllerRoutes[sessionID]; current == tr {
		delete(a.controllerRoutes, sessionID)
	}
}

func (a *AgentSession) controllerRoute(sessionID uint32) transport.Transport {
	a.routesMu.RLock()
	defer a.routesMu.RUnlock()
	return a.controllerRoutes[sessionID]
}

// StartUDPClientListener receives UDP packets from mstsc and forwards via QUIC Datagram to Agent
func (a *AgentSession) StartUDPClientListener() {
	go func() {
		buf := make([]byte, protocol.MaxUDPPacketSize)
		for {
			select {
			case <-a.ctx.Done():
				return
			default:
			}

			n, clientAddrPort, err := a.UDPListener.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}

			// Check ACL
			if a.ACLManager != nil && !a.ACLManager.IsAllowedAddr(clientAddrPort.Addr()) {
				continue
			}

			key := udp.SessionKey{
				DeviceID:   a.DeviceID,
				PublicPort: a.PublicPort,
				ClientAddr: clientAddrPort,
			}

			sess, _ := a.Sessions.GetOrCreateByKey(key)
			if sess == nil {
				continue
			}
			packetID := a.NextPacketID()

			// Fragment and transmit
			_ = a.Fragmenter.Fragment(sess.ID, packetID, buf[:n], func(datagram []byte) error {
				return a.Transport.SendDatagram(datagram)
			})
		}
	}()
}

// StartTCPClientListener accepts TCP connections from mstsc and proxies them via QUIC Stream to Agent
func (a *AgentSession) StartTCPClientListener() {
	go func() {
		for {
			clientConn, err := a.TCPListener.Accept()
			if err != nil {
				select {
				case <-a.ctx.Done():
					return
				default:
					return
				}
			}

			if tc, ok := clientConn.(*net.TCPConn); ok {
				_ = tc.SetNoDelay(true)
				_ = tc.SetKeepAlive(true)
				_ = tc.SetKeepAlivePeriod(30 * time.Second)
				_ = tc.SetReadBuffer(256 * 1024)
				_ = tc.SetWriteBuffer(256 * 1024)
			}

			var release func()
			if a.TCPGuard != nil {
				var allowed bool
				release, allowed = a.TCPGuard.Acquire(clientConn.RemoteAddr())
				if !allowed {
					_ = clientConn.Close()
					continue
				}
			}
			go func() {
				if release != nil {
					defer release()
				}
				a.handleTCPClient(clientConn)
			}()
		}
	}()
}

func (a *AgentSession) handleTCPClient(clientConn net.Conn) {
	defer clientConn.Close()

	// 1. Check ACL
	if a.ACLManager != nil && !a.ACLManager.IsAllowed(clientConn.RemoteAddr()) {
		return
	}

	// 2. Open QUIC stream to agent
	streamCtx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()

	stream, err := a.Transport.OpenStream(streamCtx)
	if err != nil {
		return
	}
	defer stream.Close()

	// 3. Write 8-byte TCP header to Agent
	connID := a.NextConnID()
	reqHdr := &protocol.TCPHeader{
		Version:      protocol.TCPProtocolVersion,
		Type:         protocol.TCPTypeOpen,
		ConnectionID: connID,
		TargetPort:   3389,
	}
	_ = stream.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := protocol.WriteTCPHeader(stream, reqHdr); err != nil {
		return
	}

	// 4. Read response header from Agent
	_ = stream.SetReadDeadline(time.Now().Add(10 * time.Second))
	respHdr, err := protocol.ReadTCPHeader(stream)
	if err != nil || respHdr.Type != protocol.TCPTypeOpenOK {
		return
	}
	_ = stream.SetDeadline(time.Time{})

	// 5. Proxy data bidirectionally
	p := tcp.NewProxy(clientConn, stream, nil)
	_ = p.Run()
}
