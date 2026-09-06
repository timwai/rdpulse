package quicgo

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"github.com/quic-go/quic-go"

	"rdpulse/internal/transport"
)

// Ensure implementation satisfies interface
var _ transport.Transport = (*QUICTransport)(nil)
var _ transport.Stream = (*quicStreamWrapper)(nil)

// udpSocketBufferBytes sizes the kernel buffers behind every QUIC socket.
// quic-go otherwise inherits the OS default, which is small enough that a burst
// of RDP frames is dropped by the kernel before quic-go ever sees it.
const udpSocketBufferBytes = 4 * 1024 * 1024

// DefaultQUICConfig returns standard QUIC configuration tuned for RDPulse
func DefaultQUICConfig() *quic.Config {
	return &quic.Config{
		EnableDatagrams:                true,
		InitialStreamReceiveWindow:     8 * 1024 * 1024,  // 8 MiB
		MaxStreamReceiveWindow:         8 * 1024 * 1024,  // 8 MiB
		InitialConnectionReceiveWindow: 32 * 1024 * 1024, // 32 MiB
		MaxConnectionReceiveWindow:     32 * 1024 * 1024, // 32 MiB
		KeepAlivePeriod:                10 * time.Second,
		MaxIdleTimeout:                 30 * time.Second,
		// Path MTU discovery grows the datagram size past the conservative
		// 1200-byte floor, which matters because RDP UDP rides in datagrams.
		DisablePathMTUDiscovery: false,
	}
}

// newTunedUDPConn binds a UDP socket with enlarged kernel buffers.
func newTunedUDPConn(addr string) (*net.UDPConn, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	_ = conn.SetReadBuffer(udpSocketBufferBytes)
	_ = conn.SetWriteBuffer(udpSocketBufferBytes)
	return conn, nil
}

type quicStreamWrapper struct {
	quic.Stream
}

func (s *quicStreamWrapper) CancelRead(code uint64) {
	s.Stream.CancelRead(quic.StreamErrorCode(code))
}

func (s *quicStreamWrapper) CancelWrite(code uint64) {
	s.Stream.CancelWrite(quic.StreamErrorCode(code))
}

// QUICTransport wraps quic.Connection into transport.Transport
type QUICTransport struct {
	conn quic.Connection
	// transport is set when this side owns the underlying socket and must
	// release it on Close.
	transport *quic.Transport
}

// NewQUICTransport wraps an existing quic.Connection
func NewQUICTransport(conn quic.Connection) *QUICTransport {
	return &QUICTransport{conn: conn}
}

func (t *QUICTransport) OpenStream(ctx context.Context) (transport.Stream, error) {
	s, err := t.conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return &quicStreamWrapper{Stream: s}, nil
}

func (t *QUICTransport) AcceptStream(ctx context.Context) (transport.Stream, error) {
	s, err := t.conn.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return &quicStreamWrapper{Stream: s}, nil
}

func (t *QUICTransport) SendDatagram(payload []byte) error {
	return t.conn.SendDatagram(payload)
}

func (t *QUICTransport) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	return t.conn.ReceiveDatagram(ctx)
}

func (t *QUICTransport) RemoteAddr() net.Addr {
	return t.conn.RemoteAddr()
}

func (t *QUICTransport) LocalAddr() net.Addr {
	return t.conn.LocalAddr()
}

func (t *QUICTransport) Stats() transport.TransportStats {
	// quic-go provides RTT information via connection or stats
	return transport.TransportStats{
		RTT: 0,
	}
}

func (t *QUICTransport) SupportsDatagrams() bool { return true }

func (t *QUICTransport) Close() error {
	err := t.conn.CloseWithError(0, "transport closed")
	if t.transport != nil {
		_ = t.transport.Close()
	}
	return err
}

// QUICListener wraps quic.Listener
type QUICListener struct {
	listener  *quic.Listener
	transport *quic.Transport
}

func ListenAddr(addr string, tlsConf *tls.Config, quicConf *quic.Config) (*QUICListener, error) {
	if quicConf == nil {
		quicConf = DefaultQUICConfig()
	}
	conn, err := newTunedUDPConn(addr)
	if err != nil {
		return nil, err
	}
	tr := &quic.Transport{Conn: conn}
	l, err := tr.Listen(tlsConf, quicConf)
	if err != nil {
		_ = tr.Close()
		_ = conn.Close()
		return nil, err
	}
	return &QUICListener{listener: l, transport: tr}, nil
}

func (l *QUICListener) Accept(ctx context.Context) (transport.Transport, error) {
	conn, err := l.listener.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return NewQUICTransport(conn), nil
}

func (l *QUICListener) Addr() net.Addr {
	return l.listener.Addr()
}

func (l *QUICListener) Close() error {
	err := l.listener.Close()
	if l.transport != nil {
		_ = l.transport.Close()
	}
	return err
}

// Dial dials a QUIC server over a socket with enlarged kernel buffers.
func Dial(ctx context.Context, addr string, tlsConf *tls.Config, quicConf *quic.Config) (transport.Transport, error) {
	if quicConf == nil {
		quicConf = DefaultQUICConfig()
	}
	remote, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	local, err := newTunedUDPConn(":0")
	if err != nil {
		return nil, err
	}
	tr := &quic.Transport{Conn: local}
	conn, err := tr.Dial(ctx, remote, tlsConf, quicConf)
	if err != nil {
		_ = tr.Close()
		_ = local.Close()
		return nil, err
	}
	return &QUICTransport{conn: conn, transport: tr}, nil
}
