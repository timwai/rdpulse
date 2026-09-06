package tcp

import (
	"errors"
	"io"
	"net"
	"time"
)

// socketBufferBytes gives each RDP connection room for a full screen update
// without stalling on kernel buffer space.
const socketBufferBytes = 256 * 1024

// TuneConn applies the socket options RDP needs. Disabling Nagle is the
// important one: RDP sends small input and cursor updates, and coalescing them
// with the peer's delayed ACK adds tens of milliseconds to every keystroke.
func TuneConn(conn net.Conn) {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tc.SetNoDelay(true)
	_ = tc.SetKeepAlive(true)
	_ = tc.SetKeepAlivePeriod(30 * time.Second)
	_ = tc.SetReadBuffer(socketBufferBytes)
	_ = tc.SetWriteBuffer(socketBufferBytes)
}

// Copy is io.Copy over a pooled buffer, so a burst of new RDP connections does
// not each allocate their own scratch space.
func Copy(dst io.Writer, src io.Reader) (int64, error) {
	bufPtr := proxyBufPool.Get().(*[]byte)
	defer proxyBufPool.Put(bufPtr)
	return io.CopyBuffer(dst, src, *bufPtr)
}

// Bridge copies bidirectionally between two connections until either side ends,
// then tears both down so neither copy goroutine is left blocked.
func Bridge(a, b net.Conn) error {
	TuneConn(a)
	TuneConn(b)

	errCh := make(chan error, 2)
	go func() {
		_, err := Copy(b, a)
		errCh <- err
	}()
	go func() {
		_, err := Copy(a, b)
		errCh <- err
	}()

	first := <-errCh
	_ = a.Close()
	_ = b.Close()
	second := <-errCh

	if isRealError(first) {
		return first
	}
	if isRealError(second) {
		return second
	}
	return nil
}

func isRealError(err error) bool {
	return err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed)
}
