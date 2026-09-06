package tcp

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"rdpulse/internal/transport"
)

var proxyBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 64*1024)
		return &b
	},
}

// ProxyStats records bytes transferred across the proxy
type ProxyStats struct {
	BytesIn  atomic.Int64
	BytesOut atomic.Int64
}

// Proxy bridges data bidirectionally between a local TCP connection and a QUIC Stream
type Proxy struct {
	tcpConn net.Conn
	stream  transport.Stream
	stats   *ProxyStats
	once    sync.Once
}

// NewProxy creates a new bidirectional Proxy
func NewProxy(tcpConn net.Conn, stream transport.Stream, stats *ProxyStats) *Proxy {
	if stats == nil {
		stats = &ProxyStats{}
	}
	return &Proxy{
		tcpConn: tcpConn,
		stream:  stream,
		stats:   stats,
	}
}

// countingReader wraps io.Reader to count bytes read
type countingReader struct {
	r       io.Reader
	counter *atomic.Int64
}

func (cr *countingReader) Read(p []byte) (n int, err error) {
	n, err = cr.r.Read(p)
	if n > 0 {
		cr.counter.Add(int64(n))
	}
	return n, err
}

// Run starts the bidirectional copy and blocks until both directions complete
func (p *Proxy) Run() error {
	errCh := make(chan error, 2)

	// Tune TCP socket if applicable (disable Nagle for low latency, expand buffers)
	if tc, ok := p.tcpConn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
		_ = tc.SetReadBuffer(256 * 1024)
		_ = tc.SetWriteBuffer(256 * 1024)
	}

	// Stream -> TCP (mstsc / local RDP)
	go func() {
		bufPtr := proxyBufPool.Get().(*[]byte)
		defer proxyBufPool.Put(bufPtr)

		reader := &countingReader{r: p.stream, counter: &p.stats.BytesIn}
		_, err := io.CopyBuffer(p.tcpConn, reader, *bufPtr)
		if tcpConn, ok := p.tcpConn.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
		errCh <- err
	}()

	// TCP -> Stream
	go func() {
		bufPtr := proxyBufPool.Get().(*[]byte)
		defer proxyBufPool.Put(bufPtr)

		reader := &countingReader{r: p.tcpConn, counter: &p.stats.BytesOut}
		_, err := io.CopyBuffer(p.stream, reader, *bufPtr)
		// Close stream write
		_ = p.stream.Close()
		errCh <- err
	}()

	// Wait for first direction to terminate
	err1 := <-errCh

	// Close both sides cleanly to ensure unblocking the second goroutine
	p.Close()

	// Wait for second direction to finish
	err2 := <-errCh

	if err1 != nil && !errors.Is(err1, io.EOF) && !errors.Is(err1, net.ErrClosed) {
		return err1
	}
	if err2 != nil && !errors.Is(err2, io.EOF) && !errors.Is(err2, net.ErrClosed) {
		return err2
	}
	return nil
}

// Close terminates both TCP and QUIC connections
func (p *Proxy) Close() {
	p.once.Do(func() {
		if p.tcpConn != nil {
			_ = p.tcpConn.Close()
		}
		if p.stream != nil {
			p.stream.CancelRead(0)
			p.stream.CancelWrite(0)
			_ = p.stream.Close()
		}
	})
}
