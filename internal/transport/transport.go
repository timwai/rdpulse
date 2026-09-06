package transport

import (
	"context"
	"io"
	"net"
	"time"
)

// Stream represents a bidirectional byte stream within a Transport connection
type Stream interface {
	io.Reader
	io.Writer
	io.Closer
	CancelRead(code uint64)
	CancelWrite(code uint64)
	SetDeadline(t time.Time) error
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}

// TransportStats contains transport connection metrics
type TransportStats struct {
	RTT time.Duration
}

// Transport is the core transport abstraction isolating upper layers from concrete QUIC implementations
type Transport interface {
	// OpenStream opens a new bidirectional stream to the remote peer
	OpenStream(ctx context.Context) (Stream, error)

	// AcceptStream accepts a bidirectional stream initiated by the remote peer
	AcceptStream(ctx context.Context) (Stream, error)

	// SendDatagram sends an unreliable datagram to the remote peer
	SendDatagram(payload []byte) error

	// ReceiveDatagram receives an unreliable datagram from the remote peer
	ReceiveDatagram(ctx context.Context) ([]byte, error)

	// RemoteAddr returns remote peer address
	RemoteAddr() net.Addr

	// LocalAddr returns local socket address
	LocalAddr() net.Addr

	// Stats returns transport level statistics
	Stats() TransportStats

	// Close closes the transport connection
	Close() error
}

// DatagramSupport lets reliable TCP fallbacks explicitly disable UDP relay.
// Unknown implementations default to true for backwards-compatible mocks.
type DatagramSupport interface {
	SupportsDatagrams() bool
}

func SupportsDatagrams(tr Transport) bool {
	if tr == nil {
		return false
	}
	capable, ok := tr.(DatagramSupport)
	return !ok || capable.SupportsDatagrams()
}

// Listener is the server-side transport listener
type Listener interface {
	Accept(ctx context.Context) (Transport, error)
	Addr() net.Addr
	Close() error
}
