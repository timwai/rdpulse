package punch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"rdpulse/internal/protocol"
)

var (
	ErrPunchTimeout = errors.New("udp punch timed out without peer response")
)

// PunchResult encapsulates the result of a successful UDP hole punch
type PunchResult struct {
	Conn       *net.UDPConn
	RemoteAddr netip.AddrPort
	SessionID  uint64
	Token      []byte
	cancelKA   context.CancelFunc
	kaMu       sync.Mutex
	packets    <-chan UDPPacket
	release    func()
	closeOnce  sync.Once
	closeErr   error
	deadlineMu sync.RWMutex
	deadline   time.Time
}

// Close closes the UDP socket and stops keepalive
func (r *PunchResult) Close() error {
	r.closeOnce.Do(func() {
		r.StopKeepalive()
		if r.release != nil {
			r.release()
			return
		}
		if r.Conn != nil {
			r.closeErr = r.Conn.Close()
		}
	})
	return r.closeErr
}

// SetReadDeadline applies a per-session deadline when the socket is shared by
// a UDPDispatcher, or delegates to the dedicated socket otherwise.
func (r *PunchResult) SetReadDeadline(deadline time.Time) error {
	if r.packets == nil {
		return r.Conn.SetReadDeadline(deadline)
	}
	r.deadlineMu.Lock()
	r.deadline = deadline
	r.deadlineMu.Unlock()
	return nil
}

// ReadFromUDP reads the next packet for this P2P session. Hot paths should
// prefer ReadFromUDPAddrPort, which does not allocate a *net.UDPAddr.
func (r *PunchResult) ReadFromUDP(buf []byte) (int, *net.UDPAddr, error) {
	n, addr, err := r.ReadFromUDPAddrPort(buf)
	if err != nil || !addr.IsValid() {
		return n, nil, err
	}
	return n, net.UDPAddrFromAddrPort(addr), nil
}

// ReadFromUDPAddrPort reads the next packet for this P2P session without
// allocating an address on the per-packet path.
func (r *PunchResult) ReadFromUDPAddrPort(buf []byte) (int, netip.AddrPort, error) {
	if r.packets == nil {
		return r.Conn.ReadFromUDPAddrPort(buf)
	}
	r.deadlineMu.RLock()
	deadline := r.deadline
	r.deadlineMu.RUnlock()
	if deadline.IsZero() {
		packet, ok := <-r.packets
		if !ok {
			return 0, netip.AddrPort{}, net.ErrClosed
		}
		return copyPacket(buf, packet)
	}
	wait := time.Until(deadline)
	if wait <= 0 {
		return 0, netip.AddrPort{}, &udpTimeoutError{}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case packet, ok := <-r.packets:
		if !ok {
			return 0, netip.AddrPort{}, net.ErrClosed
		}
		return copyPacket(buf, packet)
	case <-timer.C:
		return 0, netip.AddrPort{}, &udpTimeoutError{}
	}
}

// copyPacket drains the packet into dst and returns its pooled buffer.
func copyPacket(dst []byte, packet UDPPacket) (int, netip.AddrPort, error) {
	defer packet.release()
	if len(packet.Data) > len(dst) {
		copy(dst, packet.Data[:len(dst)])
		return len(dst), packet.Addr, nil
	}
	return copy(dst, packet.Data), packet.Addr, nil
}

type udpTimeoutError struct{}

func (*udpTimeoutError) Error() string   { return "udp read timeout" }
func (*udpTimeoutError) Timeout() bool   { return true }
func (*udpTimeoutError) Temporary() bool { return true }

// StopKeepalive stops keepalive without closing a caller-owned shared socket.
func (r *PunchResult) StopKeepalive() {
	r.kaMu.Lock()
	defer r.kaMu.Unlock()
	if r.cancelKA != nil {
		r.cancelKA()
		r.cancelKA = nil
	}
}

// StartKeepalive launches periodic keepalive packets to preserve NAT mappings
func (r *PunchResult) StartKeepalive(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	kaCtx, cancel := context.WithCancel(ctx)
	r.kaMu.Lock()
	if r.cancelKA != nil {
		r.cancelKA()
	}
	r.cancelKA = cancel
	r.kaMu.Unlock()

	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		udpAddr := net.UDPAddrFromAddrPort(r.RemoteAddr)
		for {
			select {
			case <-kaCtx.Done():
				return
			case <-ticker.C:
				pkt, err := protocol.NewPunchPacket(protocol.PunchTypeKeepalive, r.SessionID, r.Token)
				if err == nil {
					var buf [protocol.PunchPacketSize]byte
					_, _ = r.Conn.WriteToUDP(pkt.Encode(buf[:0]), udpAddr)
				}
			}
		}
	}()
}

// PunchUDP coordinates simultaneous UDP punch packet transmissions towards peer candidates
func PunchUDP(
	ctx context.Context,
	localConn *net.UDPConn,
	candidates []protocol.CandidateInfo,
	sessionID uint64,
	sessionToken []byte,
	timeout time.Duration,
) (*PunchResult, error) {
	return PunchUDPWithUpdates(ctx, localConn, candidates, nil, sessionID, sessionToken, timeout)
}

// PunchUDPWithUpdates owns a dedicated socket dispatcher for one Controller
// session and keeps consuming trickled candidates until the attempt completes.
func PunchUDPWithUpdates(ctx context.Context, localConn *net.UDPConn, candidates []protocol.CandidateInfo, candidateUpdates <-chan []protocol.CandidateInfo, sessionID uint64, sessionToken []byte, timeout time.Duration) (*PunchResult, error) {
	conn := localConn
	if conn == nil {
		var err error
		conn, err = net.ListenUDP("udp", nil)
		if err != nil {
			return nil, fmt.Errorf("listen local udp failed: %w", err)
		}
	}
	dispatcher := NewUDPDispatcher(conn)
	result, err := PunchUDPDispatched(ctx, dispatcher, candidates, candidateUpdates, sessionID, sessionToken, timeout)
	if err != nil {
		_ = dispatcher.Close()
		return nil, err
	}
	originalRelease := result.release
	result.release = func() {
		if originalRelease != nil {
			originalRelease()
		}
		_ = dispatcher.Close()
	}
	return result, nil
}

// PunchUDPDispatched punches through a shared socket owned by dispatcher.
// Candidate updates are incorporated while the punch attempt is running.
func PunchUDPDispatched(
	ctx context.Context,
	dispatcher *UDPDispatcher,
	candidates []protocol.CandidateInfo,
	candidateUpdates <-chan []protocol.CandidateInfo,
	sessionID uint64,
	sessionToken []byte,
	timeout time.Duration,
) (*PunchResult, error) {
	if dispatcher == nil || dispatcher.Conn() == nil {
		return nil, errors.New("udp dispatcher is required")
	}
	if timeout <= 0 {
		timeout = 2500 * time.Millisecond
	}
	packets, release, err := dispatcher.Register(sessionID, sessionToken)
	if err != nil {
		return nil, err
	}
	keepRegistration := false
	defer func() {
		if !keepRegistration {
			release()
		}
	}()

	punchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn := dispatcher.Conn()
	targets := resolveUDPCandidates(candidates)
	if len(targets) == 0 && candidateUpdates == nil {
		return nil, errors.New("no udp candidates available for punching")
	}

	successCh := make(chan netip.AddrPort, 1)
	var confirmedPeer atomic.Pointer[netip.AddrPort]
	var once sync.Once
	go func() {
		for {
			select {
			case <-punchCtx.Done():
				return
			case packet, ok := <-packets:
				if !ok {
					return
				}
				pkt, err := protocol.DecodePunchPacket(packet.Data, sessionToken)
				remote := packet.Addr
				packet.release()
				if err != nil || pkt.SessionID != sessionID || !remote.IsValid() {
					continue
				}
				if pkt.Type == protocol.PunchTypePunch {
					ack, ackErr := protocol.NewPunchPacket(protocol.PunchTypePunchAck, sessionID, sessionToken)
					if ackErr == nil {
						var buf [protocol.PunchPacketSize]byte
						_, _ = conn.WriteToUDPAddrPort(ack.Encode(buf[:0]), remote)
					}
				}
				if pkt.Type == protocol.PunchTypePunch || pkt.Type == protocol.PunchTypePunchAck {
					once.Do(func() {
						confirmedPeer.Store(&remote)
						successCh <- remote
					})
				}
			}
		}
	}()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case peer := <-successCh:
			ack, _ := protocol.NewPunchPacket(protocol.PunchTypePunchAck, sessionID, sessionToken)
			var buf [protocol.PunchPacketSize]byte
			target := net.UDPAddrFromAddrPort(peer)
			for i := 0; i < 3; i++ {
				_, _ = conn.WriteToUDP(ack.Encode(buf[:0]), target)
			}
			keepRegistration = true
			return &PunchResult{Conn: conn, RemoteAddr: peer, SessionID: sessionID, Token: sessionToken, packets: packets, release: release}, nil
		case update, ok := <-candidateUpdates:
			if ok {
				targets = mergeUDPAddrs(targets, resolveUDPCandidates(update))
			} else {
				candidateUpdates = nil
			}
		case <-ticker.C:
			if confirmedPeer.Load() != nil {
				continue
			}
			pkt, pktErr := protocol.NewPunchPacket(protocol.PunchTypePunch, sessionID, sessionToken)
			if pktErr != nil {
				continue
			}
			var buf [protocol.PunchPacketSize]byte
			data := pkt.Encode(buf[:0])
			for _, target := range targets {
				_, _ = conn.WriteToUDP(data, target)
			}
		case <-punchCtx.Done():
			return nil, ErrPunchTimeout
		}
	}
}

func resolveUDPCandidates(candidates []protocol.CandidateInfo) []*net.UDPAddr {
	result := make([]*net.UDPAddr, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Protocol != "udp" {
			continue
		}
		if addr, err := net.ResolveUDPAddr("udp", candidate.Address); err == nil {
			result = append(result, addr)
		}
	}
	return result
}

func mergeUDPAddrs(current, additions []*net.UDPAddr) []*net.UDPAddr {
	seen := make(map[string]struct{}, len(current)+len(additions))
	merged := make([]*net.UDPAddr, 0, len(current)+len(additions))
	for _, list := range [][]*net.UDPAddr{current, additions} {
		for _, addr := range list {
			key := addr.String()
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, addr)
		}
	}
	return merged
}
