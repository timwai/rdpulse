package tlsmux

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"time"

	"rdpulse/internal/ratelimit"
	"rdpulse/internal/tcp"
	"rdpulse/internal/transport"
)

type Listener struct {
	listener   net.Listener
	tlsConfig  *tls.Config
	accepted   chan acceptResult
	done       chan struct{}
	closeOnce  sync.Once
	guard      *ratelimit.IPGuard
	handshakes chan struct{}
}

type acceptResult struct {
	transport transport.Transport
	err       error
}

var _ transport.Listener = (*Listener)(nil)

func Listen(addr string, tlsConfig *tls.Config) (*Listener, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	config := tlsConfig.Clone()
	config.MinVersion = tls.VersionTLS13
	config.NextProtos = []string{ALPN}
	l := &Listener{
		listener:   listener,
		tlsConfig:  config,
		accepted:   make(chan acceptResult, 128),
		done:       make(chan struct{}),
		guard:      ratelimit.NewIPGuard(16, 60, time.Minute),
		handshakes: make(chan struct{}, 128),
	}
	go l.acceptLoop()
	return l, nil
}

func (l *Listener) Accept(ctx context.Context) (transport.Transport, error) {
	select {
	case result := <-l.accepted:
		return result.transport, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *Listener) Addr() net.Addr { return l.listener.Addr() }
func (l *Listener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		close(l.done)
		err = l.listener.Close()
	})
	return err
}

func (l *Listener) acceptLoop() {
	for {
		raw, err := l.listener.Accept()
		if err != nil {
			return
		}
		release, allowed := l.guard.Acquire(raw.RemoteAddr())
		if !allowed {
			_ = raw.Close()
			continue
		}
		select {
		case l.handshakes <- struct{}{}:
			go l.handshake(raw, release)
		default:
			release()
			_ = raw.Close()
		}
	}
}

func (l *Listener) handshake(raw net.Conn, release func()) {
	defer func() { <-l.handshakes }()
	defer release()
	tcp.TuneConn(raw)
	conn := tls.Server(raw, l.tlsConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := conn.HandshakeContext(ctx)
	cancel()
	if err != nil {
		_ = raw.Close()
		return
	}
	if conn.ConnectionState().NegotiatedProtocol != ALPN {
		_ = conn.Close()
		return
	}
	result := acceptResult{transport: newSession(conn, false)}
	select {
	case l.accepted <- result:
	case <-l.done:
		_ = result.transport.Close()
	}
}
