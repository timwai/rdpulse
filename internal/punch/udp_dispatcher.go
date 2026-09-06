package punch

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync"

	"rdpulse/internal/protocol"
	"rdpulse/internal/udp"
)

const dispatcherQueueSize = 256

var ErrPunchSessionRegistered = errors.New("udp punch session already registered")

// UDPPacket is a packet read by the single socket owner and routed by
// SessionID. Data is copied before delivery. Ownership of buf transfers to the
// receiver, which returns it to the pool once the payload has been consumed.
type UDPPacket struct {
	Data []byte
	Addr netip.AddrPort

	buf *[]byte
}

// release returns the packet's pooled backing buffer.
func (p *UDPPacket) release() {
	if p.buf != nil {
		udp.PutLargeBuffer(p.buf)
		p.buf = nil
	}
	p.Data = nil
}

// UDPDispatcher is the only reader for a punch socket. It prevents concurrent
// P2P sessions from racing on ReadFromUDP and routes both punch and data frames
// to their owning session.
type UDPDispatcher struct {
	conn      *net.UDPConn
	mu        sync.RWMutex
	sessions  map[uint64]*dispatchSession
	done      chan struct{}
	closeOnce sync.Once
}

type dispatchSession struct {
	packets chan UDPPacket
	key     []byte
	// codec caches the expanded HMAC key so verifying each data packet does not
	// repeat key setup on the shared read loop.
	codec *protocol.P2PCodec
}

func NewUDPDispatcher(conn *net.UDPConn) *UDPDispatcher {
	d := &UDPDispatcher{
		conn:     conn,
		sessions: make(map[uint64]*dispatchSession),
		done:     make(chan struct{}),
	}
	go d.readLoop()
	return d
}

func (d *UDPDispatcher) Conn() *net.UDPConn { return d.conn }

func (d *UDPDispatcher) Register(sessionID uint64, key []byte) (<-chan UDPPacket, func(), error) {
	if len(key) == 0 {
		return nil, nil, protocol.ErrPunchMissingKey
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.done:
		return nil, nil, net.ErrClosed
	default:
	}
	if _, exists := d.sessions[sessionID]; exists {
		return nil, nil, ErrPunchSessionRegistered
	}
	ch := make(chan UDPPacket, dispatcherQueueSize)
	entry := &dispatchSession{packets: ch, key: append([]byte(nil), key...)}
	if sessionID <= uint64(^uint32(0)) {
		entry.codec, _ = protocol.NewP2PCodec(uint32(sessionID), key)
	}
	d.sessions[sessionID] = entry
	var once sync.Once
	release := func() {
		once.Do(func() {
			d.mu.Lock()
			if current := d.sessions[sessionID]; current == entry {
				delete(d.sessions, sessionID)
				close(ch)
			}
			d.mu.Unlock()
			drainPackets(ch)
		})
	}
	return ch, release, nil
}

// drainPackets reclaims pooled buffers still queued for a released session.
func drainPackets(ch chan UDPPacket) {
	for {
		select {
		case packet, ok := <-ch:
			if !ok {
				return
			}
			packet.release()
		default:
			return
		}
	}
}

func (d *UDPDispatcher) Close() error {
	var err error
	d.closeOnce.Do(func() {
		close(d.done)
		err = d.conn.Close()
		d.mu.Lock()
		for id, entry := range d.sessions {
			delete(d.sessions, id)
			close(entry.packets)
			drainPackets(entry.packets)
		}
		d.mu.Unlock()
	})
	return err
}

func (d *UDPDispatcher) readLoop() {
	buf := make([]byte, protocol.MaxUDPPacketSize+protocol.P2PDataHeaderSize)
	for {
		n, addr, err := d.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		sessionID, ok := packetSessionID(buf[:n])
		if !ok {
			continue
		}

		d.mu.RLock()
		entry := d.sessions[sessionID]
		if entry == nil || !entry.validPacket(buf[:n], sessionID) {
			d.mu.RUnlock()
			continue
		}

		bufPtr := udp.GetLargeBuffer()
		packet := UDPPacket{
			Data: append((*bufPtr)[:0], buf[:n]...),
			Addr: addr,
			buf:  bufPtr,
		}
		select {
		case entry.packets <- packet:
		default:
			// UDP is intentionally lossy. A bounded queue prevents one stalled
			// session from blocking every other session on the shared socket.
			packet.release()
		}
		d.mu.RUnlock()
	}
}

func (e *dispatchSession) validPacket(packet []byte, sessionID uint64) bool {
	if len(packet) < 4 {
		return false
	}
	switch binary.BigEndian.Uint32(packet[0:4]) {
	case protocol.PunchMagic:
		decoded, err := protocol.DecodePunchPacket(packet, e.key)
		return err == nil && decoded.SessionID == sessionID
	case protocol.P2PDataMagic:
		if e.codec == nil {
			return false
		}
		_, err := e.codec.Decode(packet)
		return err == nil
	default:
		return false
	}
}

func packetSessionID(packet []byte) (uint64, bool) {
	if len(packet) < 8 {
		return 0, false
	}
	switch binary.BigEndian.Uint32(packet[0:4]) {
	case protocol.PunchMagic:
		if len(packet) < protocol.PunchPacketSize {
			return 0, false
		}
		return binary.BigEndian.Uint64(packet[8:16]), true
	case protocol.P2PDataMagic:
		return uint64(binary.BigEndian.Uint32(packet[4:8])), true
	default:
		return 0, false
	}
}
