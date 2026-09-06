// Package tlsmux implements the RDPulse Transport interface over TLS/TCP.
// It is an independent fallback for networks that block QUIC/UDP.
package tlsmux

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"rdpulse/internal/tcp"
	"rdpulse/internal/transport"
)

const (
	ALPN = "rdpulse-tlsmux"

	frameOpen     = 1
	frameData     = 2
	frameClose    = 3
	frameDatagram = 4

	frameHeaderSize = 9
	maxFramePayload = 64 * 1024

	// maxWriteChunk bounds how long one stream can hold the shared write token.
	// A full 64 KiB frame on a slow link stalls every other stream, including
	// control frames, for the duration of that single write; 16 KiB keeps the
	// worst-case wait to a quarter of that. Peers still accept maxFramePayload,
	// so lowering only what we send stays wire-compatible.
	maxWriteChunk = 16 * 1024

	maxStreams        = 256
	streamQueueSize   = 64
	datagramQueueSize = 256
)

var (
	ErrBadFrame = errors.New("invalid TLS mux frame")
	ErrTimeout  = &timeoutError{}
)

// framePool backs both directions of the mux. Frames are written to a TLS
// connection, so the header and payload must go out in one Write to land in a
// single TLS record; that means one contiguous buffer rather than a writev.
var framePool = sync.Pool{
	New: func() any {
		b := make([]byte, frameHeaderSize+maxFramePayload)
		return &b
	},
}

func getFrameBuf() *[]byte { return framePool.Get().(*[]byte) }

func putFrameBuf(b *[]byte) {
	if b == nil || cap(*b) < frameHeaderSize+maxFramePayload {
		return
	}
	*b = (*b)[:frameHeaderSize+maxFramePayload]
	framePool.Put(b)
}

// framePayload carries a received payload together with its pooled backing
// buffer, so the reader can return the buffer once it has been drained.
type framePayload struct {
	data []byte
	buf  *[]byte
}

func (f *framePayload) release() {
	putFrameBuf(f.buf)
	f.buf = nil
	f.data = nil
}

type timeoutError struct{}

func (*timeoutError) Error() string   { return "TLS mux operation timed out" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

type session struct {
	conn       net.Conn
	client     bool
	nextID     atomic.Uint32
	streamsMu  sync.RWMutex
	streams    map[uint32]*muxStream
	acceptCh   chan *muxStream
	datagrams  chan []byte
	writeToken chan struct{}
	done       chan struct{}
	closeOnce  sync.Once
}

var _ transport.Transport = (*session)(nil)
var _ transport.Stream = (*muxStream)(nil)

func newSession(conn net.Conn, client bool) *session {
	s := &session{
		conn:       conn,
		client:     client,
		streams:    make(map[uint32]*muxStream),
		acceptCh:   make(chan *muxStream, 128),
		datagrams:  make(chan []byte, datagramQueueSize),
		writeToken: make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	if client {
		s.nextID.Store(1)
	} else {
		s.nextID.Store(2)
	}
	s.writeToken <- struct{}{}
	go s.readLoop()
	return s
}

func (s *session) OpenStream(ctx context.Context) (transport.Stream, error) {
	id := s.nextID.Add(2) - 2
	stream := newMuxStream(s, id)
	s.streamsMu.Lock()
	if len(s.streams) >= maxStreams {
		s.streamsMu.Unlock()
		return nil, errors.New("TLS mux stream limit exceeded")
	}
	s.streams[id] = stream
	s.streamsMu.Unlock()
	if err := s.writeFrame(ctx, time.Time{}, frameOpen, id, nil); err != nil {
		s.removeStream(id, stream)
		stream.closeLocal()
		return nil, err
	}
	return stream, nil
}

func (s *session) AcceptStream(ctx context.Context) (transport.Stream, error) {
	select {
	case stream := <-s.acceptCh:
		return stream, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, net.ErrClosed
	}
}

func (s *session) SendDatagram(payload []byte) error {
	if len(payload) > maxFramePayload {
		return ErrBadFrame
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.writeFrame(ctx, time.Time{}, frameDatagram, 0, payload)
}

func (s *session) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case payload := <-s.datagrams:
		return payload, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, net.ErrClosed
	}
}

func (s *session) RemoteAddr() net.Addr            { return s.conn.RemoteAddr() }
func (s *session) LocalAddr() net.Addr             { return s.conn.LocalAddr() }
func (s *session) Stats() transport.TransportStats { return transport.TransportStats{} }
func (s *session) SupportsDatagrams() bool         { return false }

func (s *session) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.conn.Close()
	})
	return nil
}

func (s *session) readLoop() {
	defer s.Close()
	var header [frameHeaderSize]byte
	for {
		if _, err := io.ReadFull(s.conn, header[:]); err != nil {
			return
		}
		frameType := header[0]
		streamID := binary.BigEndian.Uint32(header[1:5])
		length := binary.BigEndian.Uint32(header[5:9])
		if length > maxFramePayload {
			return
		}

		switch frameType {
		case frameOpen:
			if streamID == 0 || length != 0 || !s.remoteOwnsID(streamID) {
				return
			}
			stream := newMuxStream(s, streamID)
			s.streamsMu.Lock()
			if _, exists := s.streams[streamID]; exists || len(s.streams) >= maxStreams {
				s.streamsMu.Unlock()
				return
			}
			s.streams[streamID] = stream
			s.streamsMu.Unlock()
			select {
			case s.acceptCh <- stream:
			case <-s.done:
				return
			}

		case frameData:
			bufPtr := getFrameBuf()
			payload := (*bufPtr)[:int(length)]
			if _, err := io.ReadFull(s.conn, payload); err != nil {
				putFrameBuf(bufPtr)
				return
			}
			frame := framePayload{data: payload, buf: bufPtr}
			stream := s.getStream(streamID)
			if stream == nil {
				frame.release()
				continue
			}
			select {
			case stream.incoming <- frame:
			case <-stream.localDone:
				frame.release()
			case <-s.done:
				frame.release()
				return
			}

		case frameClose:
			if length != 0 {
				return
			}
			if stream := s.getStream(streamID); stream != nil {
				stream.closeRemote()
			}

		case frameDatagram:
			if streamID != 0 {
				return
			}
			// Datagrams are handed to the caller, so they keep their own buffer.
			payload := make([]byte, int(length))
			if _, err := io.ReadFull(s.conn, payload); err != nil {
				return
			}
			select {
			case s.datagrams <- payload:
			default:
				// Preserve datagram semantics under backpressure.
			}

		default:
			return
		}

		// Only data and datagram frames carry a payload; anything else with a
		// non-zero length has already been rejected above.
	}
}

func (s *session) remoteOwnsID(id uint32) bool {
	if s.client {
		return id%2 == 0
	}
	return id%2 == 1
}

func (s *session) getStream(id uint32) *muxStream {
	s.streamsMu.RLock()
	stream := s.streams[id]
	s.streamsMu.RUnlock()
	return stream
}

func (s *session) removeStream(id uint32, stream *muxStream) {
	s.streamsMu.Lock()
	if s.streams[id] == stream {
		delete(s.streams, id)
	}
	s.streamsMu.Unlock()
}

func (s *session) writeFrame(ctx context.Context, deadline time.Time, frameType byte, streamID uint32, payload []byte) error {
	if len(payload) > maxFramePayload {
		return ErrBadFrame
	}
	deadlineC, stop := deadlineChannel(deadline)
	defer stop()
	select {
	case <-s.writeToken:
	case <-ctx.Done():
		return ctx.Err()
	case <-deadlineC:
		return ErrTimeout
	case <-s.done:
		return net.ErrClosed
	}
	defer func() { s.writeToken <- struct{}{} }()

	if !deadline.IsZero() {
		_ = s.conn.SetWriteDeadline(deadline)
		defer s.conn.SetWriteDeadline(time.Time{})
	}
	bufPtr := getFrameBuf()
	defer putFrameBuf(bufPtr)

	frame := (*bufPtr)[:frameHeaderSize+len(payload)]
	frame[0] = frameType
	binary.BigEndian.PutUint32(frame[1:5], streamID)
	binary.BigEndian.PutUint32(frame[5:9], uint32(len(payload)))
	copy(frame[frameHeaderSize:], payload)
	_, err := writeAll(s.conn, frame)
	return err
}

func writeAll(writer io.Writer, data []byte) (int, error) {
	total := 0
	for len(data) > 0 {
		n, err := writer.Write(data)
		total += n
		data = data[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrUnexpectedEOF
		}
	}
	return total, nil
}

type muxStream struct {
	session       *session
	id            uint32
	incoming      chan framePayload
	current       framePayload
	localDone     chan struct{}
	remoteDone    chan struct{}
	localOnce     sync.Once
	remoteOnce    sync.Once
	readDeadline  atomic.Int64
	writeDeadline atomic.Int64
}

func newMuxStream(session *session, id uint32) *muxStream {
	return &muxStream{
		session:    session,
		id:         id,
		incoming:   make(chan framePayload, streamQueueSize),
		localDone:  make(chan struct{}),
		remoteDone: make(chan struct{}),
	}
}

func (s *muxStream) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	for len(s.current.data) == 0 {
		s.current.release()
		select {
		case frame := <-s.incoming:
			s.current = frame
			continue
		default:
		}
		deadlineC, stop := deadlineChannel(loadDeadline(&s.readDeadline))
		select {
		case frame := <-s.incoming:
			stop()
			s.current = frame
		case <-s.remoteDone:
			stop()
			return 0, io.EOF
		case <-s.localDone:
			stop()
			return 0, net.ErrClosed
		case <-s.session.done:
			stop()
			return 0, net.ErrClosed
		case <-deadlineC:
			stop()
			return 0, ErrTimeout
		}
	}
	n := copy(dst, s.current.data)
	s.current.data = s.current.data[n:]
	if len(s.current.data) == 0 {
		s.current.release()
	}
	return n, nil
}

func (s *muxStream) Write(data []byte) (int, error) {
	select {
	case <-s.localDone:
		return 0, net.ErrClosed
	case <-s.remoteDone:
		return 0, io.ErrClosedPipe
	default:
	}
	total := 0
	for len(data) > 0 {
		chunkSize := len(data)
		if chunkSize > maxWriteChunk {
			chunkSize = maxWriteChunk
		}
		ctx := context.Background()
		if err := s.session.writeFrame(ctx, loadDeadline(&s.writeDeadline), frameData, s.id, data[:chunkSize]); err != nil {
			return total, err
		}
		total += chunkSize
		data = data[chunkSize:]
	}
	return total, nil
}

func (s *muxStream) Close() error {
	s.localOnce.Do(func() {
		close(s.localDone)
		s.session.removeStream(s.id, s)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.session.writeFrame(ctx, time.Time{}, frameClose, s.id, nil)
	})
	return nil
}

func (s *muxStream) closeLocal()        { s.localOnce.Do(func() { close(s.localDone) }) }
func (s *muxStream) closeRemote()       { s.remoteOnce.Do(func() { close(s.remoteDone) }) }
func (s *muxStream) CancelRead(uint64)  { _ = s.Close() }
func (s *muxStream) CancelWrite(uint64) { _ = s.Close() }

func (s *muxStream) SetDeadline(deadline time.Time) error {
	_ = s.SetReadDeadline(deadline)
	return s.SetWriteDeadline(deadline)
}
func (s *muxStream) SetReadDeadline(deadline time.Time) error {
	storeDeadline(&s.readDeadline, deadline)
	return nil
}
func (s *muxStream) SetWriteDeadline(deadline time.Time) error {
	storeDeadline(&s.writeDeadline, deadline)
	return nil
}

func storeDeadline(target *atomic.Int64, deadline time.Time) {
	if deadline.IsZero() {
		target.Store(0)
		return
	}
	target.Store(deadline.UnixNano())
}

func loadDeadline(source *atomic.Int64) time.Time {
	nanos := source.Load()
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos)
}

func deadlineChannel(deadline time.Time) (<-chan time.Time, func()) {
	if deadline.IsZero() {
		return nil, func() {}
	}
	wait := time.Until(deadline)
	if wait < 0 {
		wait = 0
	}
	timer := time.NewTimer(wait)
	return timer.C, func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

// Dial establishes a TLS 1.3 fallback transport.
func Dial(ctx context.Context, addr string, tlsConfig *tls.Config) (transport.Transport, error) {
	dialer := &net.Dialer{}
	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	tcp.TuneConn(raw)
	config := tlsConfig.Clone()
	config.MinVersion = tls.VersionTLS13
	config.NextProtos = []string{ALPN}
	if config.ServerName == "" {
		host, _, splitErr := net.SplitHostPort(addr)
		if splitErr != nil {
			_ = raw.Close()
			return nil, splitErr
		}
		config.ServerName = host
	}
	conn := tls.Client(raw, config)
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	if conn.ConnectionState().NegotiatedProtocol != ALPN {
		_ = conn.Close()
		return nil, errors.New("TLS mux ALPN was not negotiated")
	}
	return newSession(conn, true), nil
}
