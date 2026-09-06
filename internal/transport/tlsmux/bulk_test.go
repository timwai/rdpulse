package tlsmux

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"io"
	"sync"
	"testing"
	"time"

	"rdpulse/internal/transport"
	"rdpulse/internal/transport/quicgo"
)

// dialMuxPair returns a connected client/server pair over the loopback.
func dialMuxPair(t *testing.T, ctx context.Context) (transport.Transport, transport.Transport) {
	t.Helper()

	serverTLS, err := quicgo.GenerateSelfSignedCert()
	if err != nil {
		t.Fatalf("GenerateSelfSignedCert: %v", err)
	}
	listener, err := Listen("127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	serverCh := make(chan transport.Transport, 1)
	go func() {
		server, acceptErr := listener.Accept(ctx)
		if acceptErr == nil {
			serverCh <- server
		}
	}()

	client, err := Dial(ctx, listener.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	select {
	case server := <-serverCh:
		t.Cleanup(func() { server.Close() })
		return client, server
	case <-ctx.Done():
		t.Fatal("server never accepted the mux connection")
		return nil, nil
	}
}

// TestTLSMuxBulkTransferIsByteExact guards the pooled frame buffers: a recycled
// buffer handed out again too early would corrupt the stream, which only shows
// up on payloads larger than one frame.
func TestTLSMuxBulkTransferIsByteExact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, server := dialMuxPair(t, ctx)

	clientStream, err := client.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	serverStream, err := server.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}

	// Several times maxWriteChunk, at a size that is not a frame multiple.
	payload := make([]byte, 5*maxWriteChunk+1237)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	var writeErr error
	go func() {
		defer wg.Done()
		_, writeErr = clientStream.Write(payload)
		_ = clientStream.Close()
	}()

	received := make([]byte, len(payload))
	if _, err := io.ReadFull(serverStream, received); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	wg.Wait()
	if writeErr != nil {
		t.Fatalf("Write: %v", writeErr)
	}
	if !bytes.Equal(received, payload) {
		t.Fatal("bulk transfer corrupted: received bytes differ from sent bytes")
	}
}

// TestTLSMuxConcurrentStreamsStayIndependent checks that recycled buffers are
// not shared across streams and that one stream's traffic cannot appear on
// another.
func TestTLSMuxConcurrentStreamsStayIndependent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, server := dialMuxPair(t, ctx)

	const streamCount = 8
	const chunkSize = 3 * maxWriteChunk

	payloads := make([][]byte, streamCount)
	clientStreams := make([]transport.Stream, streamCount)
	for i := range payloads {
		payloads[i] = bytes.Repeat([]byte{byte('A' + i)}, chunkSize)
		stream, err := client.OpenStream(ctx)
		if err != nil {
			t.Fatalf("OpenStream %d: %v", i, err)
		}
		clientStreams[i] = stream
	}

	serverStreams := make([]transport.Stream, streamCount)
	for i := range serverStreams {
		stream, err := server.AcceptStream(ctx)
		if err != nil {
			t.Fatalf("AcceptStream %d: %v", i, err)
		}
		serverStreams[i] = stream
	}

	var wg sync.WaitGroup
	for i := range clientStreams {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _ = clientStreams[idx].Write(payloads[idx])
		}(i)
	}

	// Each server stream must see exactly the bytes its peer wrote. Which
	// server stream pairs with which client stream is decided by open order,
	// so verify every stream carries a single repeated byte of the right count.
	errs := make(chan error, streamCount)
	seen := make([]byte, streamCount)
	var seenMu sync.Mutex
	for i := range serverStreams {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			buf := make([]byte, chunkSize)
			if _, err := io.ReadFull(serverStreams[idx], buf); err != nil {
				errs <- err
				return
			}
			want := bytes.Repeat(buf[:1], chunkSize)
			if !bytes.Equal(buf, want) {
				errs <- io.ErrUnexpectedEOF
				return
			}
			seenMu.Lock()
			seen[idx] = buf[0]
			seenMu.Unlock()
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent stream read: %v", err)
	}

	distinct := map[byte]bool{}
	for _, b := range seen {
		if distinct[b] {
			t.Fatalf("byte %q arrived on more than one stream: buffers are being shared", b)
		}
		distinct[b] = true
	}
}
