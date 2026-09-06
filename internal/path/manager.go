package path

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"rdpulse/internal/protocol"
	"rdpulse/internal/punch"
	"rdpulse/internal/transport"
	"rdpulse/internal/udp"
)

// PathType defines the underlying network transport path
type PathType uint8

const (
	PathUnknown PathType = iota
	PathDirectLAN
	PathDirectUDP
	PathDirectTCP
	PathRelay
	PathDisabled
)

// PathQUICRelay is retained as a source-compatible alias. Relay traffic may
// now run over QUIC or the independent TLS/TCP fallback transport.
const PathQUICRelay = PathRelay

func (p PathType) String() string {
	switch p {
	case PathDirectLAN:
		return "LAN Direct"
	case PathDirectUDP:
		return "UDP P2P"
	case PathDirectTCP:
		return "TCP P2P"
	case PathRelay:
		return "Relay (QUIC/TLS)"
	case PathDisabled:
		return "Disabled"
	default:
		return "Unknown"
	}
}

// Manager handles parallel path racing (Happy-Eyeballs) and dynamic path selection
type Manager struct {
	mu sync.RWMutex

	tcpPath atomic.Uint32
	udpPath atomic.Uint32

	// P2P Resources
	directUDPPunch   *punch.PunchResult
	tcpCandidates    []protocol.CandidateInfo
	candidateUpdates chan []protocol.CandidateInfo

	// Relay Resources
	relayTransport transport.Transport
	relayPublicTCP string
	relayPublicUDP string

	// UDP Data Engine
	fragmenter   *udp.Fragmenter
	reassembler  *udp.Reassembler
	replayWindow udp.ReplayWindow
	nextPacketID atomic.Uint32
	sessionID    uint32
	sessionToken []byte
	p2pCodec     *protocol.P2PCodec

	// recvBuf and assembleBuf back the single ReceiveUDP consumer, keeping the
	// per-packet receive path allocation-free. They must not be touched by
	// other goroutines, and the slice ReceiveUDP returns stays valid only
	// until the next call.
	recvBuf     []byte
	assembleBuf []byte

	// pathChanged is closed and replaced whenever a path is selected, so
	// waiters wake the instant a path is usable instead of polling for it.
	pathChangedMu sync.Mutex
	pathChanged   chan struct{}

	closed atomic.Bool
	ctx    context.Context
	cancel context.CancelFunc
}

// pathWaiter returns a channel closed on the next path change.
func (m *Manager) pathWaiter() <-chan struct{} {
	m.pathChangedMu.Lock()
	defer m.pathChangedMu.Unlock()
	if m.pathChanged == nil {
		m.pathChanged = make(chan struct{})
	}
	return m.pathChanged
}

// notifyPathChanged wakes everything blocked on the current path selection.
func (m *Manager) notifyPathChanged() {
	m.pathChangedMu.Lock()
	if m.pathChanged != nil {
		close(m.pathChanged)
		m.pathChanged = nil
	}
	m.pathChangedMu.Unlock()
}

// NewManager creates a PathManager
func NewManager(ctx context.Context, sessionID uint32, sessionToken []byte, relayTr transport.Transport) *Manager {
	mgrCtx, cancel := context.WithCancel(ctx)
	m := &Manager{
		relayTransport:   relayTr,
		fragmenter:       udp.NewFragmenter(protocol.DefaultMaxUDPPayload),
		reassembler:      udp.NewReassembler(100 * time.Millisecond),
		sessionID:        sessionID,
		sessionToken:     append([]byte(nil), sessionToken...),
		candidateUpdates: make(chan []protocol.CandidateInfo, 8),
		ctx:              mgrCtx,
		cancel:           cancel,
	}
	m.tcpPath.Store(uint32(PathUnknown))
	m.udpPath.Store(uint32(PathUnknown))
	m.recvBuf = make([]byte, protocol.MaxUDPPacketSize+protocol.P2PDataHeaderSize)
	m.assembleBuf = make([]byte, 0, protocol.MaxUDPPacketSize)
	if codec, err := protocol.NewP2PCodec(sessionID, m.sessionToken); err == nil {
		m.p2pCodec = codec
	}
	m.reassembler.StartCleaner(mgrCtx, 50*time.Millisecond)
	return m
}

// SetRelayEndpoints sets public TCP and UDP relay endpoints
func (m *Manager) SetRelayEndpoints(tcpAddr, udpAddr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.relayPublicTCP = tcpAddr
	m.relayPublicUDP = udpAddr
}

// RelayEndpoints returns public TCP and UDP relay endpoints
func (m *Manager) RelayEndpoints() (string, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.relayPublicTCP, m.relayPublicUDP
}

// SetDirectUDP establishes direct UDP P2P as the primary UDP path
func (m *Manager) SetDirectUDP(res *punch.PunchResult, isLAN bool) {
	if m.closed.Load() || res == nil {
		if res != nil {
			_ = res.Close()
		}
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed.Load() {
		_ = res.Close()
		return
	}

	m.directUDPPunch = res
	if isLAN {
		m.udpPath.Store(uint32(PathDirectLAN))
	} else {
		m.udpPath.Store(uint32(PathDirectUDP))
	}
	res.StartKeepalive(m.ctx, 15*time.Second)
	m.notifyPathChanged()
	log.Printf("[Path] UDP 升级为 %s，对端 %s", PathType(m.udpPath.Load()), res.RemoteAddr)
}

// SetDirectTCP marks TCP as an authenticated direct path after a successful punch/probe.
func (m *Manager) SetDirectTCP(isLAN bool) {
	if m.closed.Load() {
		return
	}
	if isLAN {
		m.tcpPath.Store(uint32(PathDirectLAN))
	} else {
		m.tcpPath.Store(uint32(PathDirectTCP))
	}
	m.notifyPathChanged()
	log.Printf("[Path] TCP 升级为 %s", PathType(m.tcpPath.Load()))
}

// FallbackUDPFromDirect drops a dead P2P UDP path and returns to Relay or Disabled.
func (m *Manager) FallbackUDPFromDirect() {
	m.mu.Lock()
	defer m.mu.Unlock()

	current := PathType(m.udpPath.Load())
	if current != PathDirectLAN && current != PathDirectUDP {
		return
	}
	if m.directUDPPunch != nil {
		_ = m.directUDPPunch.Close()
		m.directUDPPunch = nil
	}
	if transport.SupportsDatagrams(m.relayTransport) {
		m.udpPath.Store(uint32(PathRelay))
	} else {
		m.udpPath.Store(uint32(PathDisabled))
	}
	m.notifyPathChanged()
	log.Printf("[Path] UDP 回退为 %s", PathType(m.udpPath.Load()))
}

// AddCandidates updates direct path endpoints and wakes an in-progress UDP
// punch attempt. TCP connections are deliberately dialed per local RDP
// connection and are never shared.
func (m *Manager) AddCandidates(candidates []protocol.CandidateInfo) {
	m.mu.Lock()
	seen := make(map[string]struct{}, len(m.tcpCandidates)+len(candidates))
	for _, candidate := range m.tcpCandidates {
		seen[candidate.Protocol+"|"+candidate.Address] = struct{}{}
	}
	for _, candidate := range candidates {
		if candidate.Protocol != "tcp" {
			continue
		}
		key := candidate.Protocol + "|" + candidate.Address
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		m.tcpCandidates = append(m.tcpCandidates, candidate)
	}
	m.mu.Unlock()
	select {
	case m.candidateUpdates <- append([]protocol.CandidateInfo(nil), candidates...):
	default:
	}
}

// SetQUICRelay sets QUIC Relay as path
func (m *Manager) SetQUICRelay() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.tcpPath.Load() == uint32(PathUnknown) {
		m.tcpPath.Store(uint32(PathRelay))
	}
	if m.udpPath.Load() == uint32(PathUnknown) {
		if transport.SupportsDatagrams(m.relayTransport) {
			m.udpPath.Store(uint32(PathRelay))
		} else {
			m.udpPath.Store(uint32(PathDisabled))
		}
	}
	m.notifyPathChanged()
	log.Printf("[Path] 中继已预热: TCP=%s, UDP=%s", PathType(m.tcpPath.Load()), PathType(m.udpPath.Load()))
}

// TCPPath returns current active TCP path
func (m *Manager) TCPPath() PathType {
	return PathType(m.tcpPath.Load())
}

// UDPPath returns current active UDP path
func (m *Manager) UDPPath() PathType {
	return PathType(m.udpPath.Load())
}

// DialDirectTCP creates a fresh authenticated direct connection for one local
// RDP TCP connection.
func (m *Manager) DialDirectTCP(ctx context.Context) (net.Conn, error) {
	m.mu.RLock()
	candidates := append([]protocol.CandidateInfo(nil), m.tcpCandidates...)
	m.mu.RUnlock()
	if len(candidates) == 0 {
		return nil, errors.New("direct tcp candidates unavailable")
	}
	return punch.PunchTCP(ctx, candidates, uint64(m.sessionID), m.sessionToken, 2500*time.Millisecond)
}

// SendUDP transmits a UDP packet over the best active path
func (m *Manager) SendUDP(payload []byte) error {
	path := m.UDPPath()
	if path == PathDisabled {
		return errors.New("udp path disabled")
	}

	packetID := m.nextPacketID.Add(1)

	switch path {
	case PathDirectLAN, PathDirectUDP:
		m.mu.RLock()
		punchRes := m.directUDPPunch
		m.mu.RUnlock()

		if punchRes == nil || punchRes.Conn == nil {
			m.FallbackUDPFromDirect()
			return m.SendUDP(payload)
		}

		codec := m.p2pCodec
		if codec == nil {
			return errors.New("p2p session codec unavailable")
		}

		target := net.UDPAddrFromAddrPort(punchRes.RemoteAddr)
		sendBuf := udp.GetBuffer()
		err := m.fragmenter.Fragment(m.sessionID, packetID, payload, func(datagram []byte) error {
			packet := codec.Encode((*sendBuf)[:0], datagram)
			_, writeErr := punchRes.Conn.WriteToUDP(packet, target)
			return writeErr
		})
		udp.PutBuffer(sendBuf)
		if err != nil {
			m.FallbackUDPFromDirect()
			return m.SendUDP(payload)
		}
		return nil

	case PathRelay:
		if m.relayTransport != nil {
			return m.fragmenter.Fragment(m.sessionID, packetID, payload, func(datagram []byte) error {
				return m.relayTransport.SendDatagram(datagram)
			})
		}
		return errors.New("relay transport unavailable")

	default:
		return fmt.Errorf("no valid udp path currently active (state: %s)", path)
	}
}

// ReceiveUDP receives an assembled UDP packet from either P2P or Relay
func (m *Manager) ReceiveUDP(ctx context.Context) ([]byte, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-m.ctx.Done():
			return nil, m.ctx.Err()
		default:
		}

		path := m.UDPPath()
		switch path {
		case PathDirectLAN, PathDirectUDP:
			m.mu.RLock()
			punchRes := m.directUDPPunch
			m.mu.RUnlock()

			if punchRes == nil || punchRes.Conn == nil {
				m.FallbackUDPFromDirect()
				continue
			}

			codec := m.p2pCodec
			if codec == nil {
				m.FallbackUDPFromDirect()
				continue
			}

			_ = punchRes.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, remoteAddr, err := punchRes.ReadFromUDP(m.recvBuf)
			if err != nil {
				continue
			}
			if !sameAddrPort(remoteAddr.AddrPort(), punchRes.RemoteAddr) {
				continue
			}

			// Ignore keepalive packets
			if n >= protocol.PunchPacketSize {
				if magic := m.recvBuf[0:4]; string(magic) == "RDPU" {
					continue
				}
			}

			datagram, err := codec.Decode(m.recvBuf[:n])
			if err != nil {
				continue
			}

			hdr, fragPayload, err := protocol.DecodeUDP(datagram)
			if err != nil {
				continue
			}
			if m.replayWindow.Seen(hdr.PacketID) {
				continue
			}

			assembled, err := m.reassembler.FeedAppend(m.assembleBuf, &hdr, fragPayload)
			if err != nil || assembled == nil {
				continue
			}
			if !m.replayWindow.MarkCompleted(hdr.PacketID) {
				continue
			}
			return assembled, nil

		case PathRelay:
			if m.relayTransport == nil {
				if err := m.awaitPathChange(ctx); err != nil {
					return nil, err
				}
				continue
			}

			datagram, err := m.relayTransport.ReceiveDatagram(ctx)
			if err != nil {
				return nil, err
			}

			hdr, fragPayload, err := protocol.DecodeUDP(datagram)
			if err != nil {
				continue
			}

			assembled, err := m.reassembler.FeedAppend(m.assembleBuf, &hdr, fragPayload)
			if err != nil || assembled == nil {
				continue
			}
			return assembled, nil

		case PathDisabled:
			<-ctx.Done()
			return nil, ctx.Err()

		default:
			if err := m.awaitPathChange(ctx); err != nil {
				return nil, err
			}
		}
	}
}

// awaitPathChange blocks until a path is selected. Registering the waiter
// before re-reading the path closes the window where a change lands between
// the two and would otherwise be missed.
func (m *Manager) awaitPathChange(ctx context.Context) error {
	waiter := m.pathWaiter()
	if PathType(m.udpPath.Load()) != PathUnknown && m.relayTransport != nil {
		return nil
	}
	select {
	case <-waiter:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-m.ctx.Done():
		return m.ctx.Err()
	}
}

func sameAddrPort(a, b netip.AddrPort) bool {
	return a.Port() == b.Port() && a.Addr().Unmap() == b.Addr().Unmap()
}

// Close releases all path resources
func (m *Manager) Close() error {
	if m.closed.Swap(true) {
		return nil
	}
	m.cancel()

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.directUDPPunch != nil {
		_ = m.directUDPPunch.Close()
	}
	return nil
}

// ParallelRace coordinates Happy-Eyeballs competition between P2P and Relay fallback
func ParallelRace(
	ctx context.Context,
	localUDPConn *net.UDPConn,
	candidates []protocol.CandidateInfo,
	sessionID uint64,
	sessionToken []byte,
	relayFallbackFn func() error,
	pathMgr *Manager,
) {
	raceCtx, raceCancel := context.WithCancel(pathMgr.ctx)
	pathMgr.AddCandidates(candidates)
	log.Printf("[Path] 路径竞速开始 Session=%d 候选=%s", sessionID, protocol.FormatCandidates(candidates))

	// T = 0ms: Start UDP P2P punch
	go func() {
		defer raceCancel()
		res, err := punch.PunchUDPWithUpdates(raceCtx, localUDPConn, candidates, pathMgr.candidateUpdates, sessionID, sessionToken, 2500*time.Millisecond)
		if err == nil && res != nil {
			isLAN := isCandidateLAN(res.RemoteAddr, candidates)
			log.Printf("[Path] UDP 竞速成功 对端=%s LAN=%v", res.RemoteAddr, isLAN)
			pathMgr.SetDirectUDP(res, isLAN)
		} else {
			if err != nil {
				log.Printf("[Path] UDP 竞速未成功: %v", err)
			}
			if localUDPConn != nil {
				_ = localUDPConn.Close()
			}
		}
	}()

	go func() {
		if err := punch.ProbeTCP(pathMgr.ctx, candidates, sessionID, sessionToken, 2500*time.Millisecond); err == nil {
			isLAN := tcpCandidatesAreLAN(candidates)
			log.Printf("[Path] TCP 探测成功 LAN=%v", isLAN)
			pathMgr.SetDirectTCP(isLAN)
		} else {
			log.Printf("[Path] TCP 探测未成功: %v", err)
		}
	}()

	// T = 300ms: if P2P is still unconfirmed, activate the warm Relay path
	go func() {
		select {
		case <-pathMgr.ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
			if pathMgr.TCPPath() == PathUnknown || pathMgr.UDPPath() == PathUnknown {
				if relayFallbackFn != nil {
					_ = relayFallbackFn()
				}
				pathMgr.SetQUICRelay()
			}
		}
	}()

}

func isCandidateLAN(addr netip.AddrPort, candidates []protocol.CandidateInfo) bool {
	for _, c := range candidates {
		if c.Type != "lan" {
			continue
		}
		parsed, err := netip.ParseAddrPort(c.Address)
		if err != nil {
			continue
		}
		if sameAddrPort(addr, parsed) {
			return true
		}
	}
	return false
}

func tcpCandidatesAreLAN(candidates []protocol.CandidateInfo) bool {
	for _, candidate := range candidates {
		if candidate.Protocol == "tcp" && candidate.Type == "lan" {
			return true
		}
	}
	return false
}
