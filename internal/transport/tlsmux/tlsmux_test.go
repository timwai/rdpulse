package tlsmux

import (
	"context"
	"crypto/tls"
	"io"
	"testing"
	"time"

	"rdpulse/internal/transport"
	"rdpulse/internal/transport/quicgo"
)

func TestTLSMuxStreamsBidirectional(t *testing.T) {
	serverTLS, err := quicgo.GenerateSelfSignedCert()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := Listen("127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverCh := make(chan transport.Transport, 1)
	go func() {
		server, acceptErr := listener.Accept(ctx)
		if acceptErr == nil {
			serverCh <- server
		}
	}()
	client, err := Dial(ctx, listener.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-serverCh
	defer server.Close()

	clientStream, err := client.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	serverStream, err := server.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientStream.Write([]byte("stream payload")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("stream payload"))
	if _, err := io.ReadFull(serverStream, buf); err != nil || string(buf) != "stream payload" {
		t.Fatalf("stream read=%q err=%v", buf, err)
	}

	if client.(*session).SupportsDatagrams() || server.(*session).SupportsDatagrams() {
		t.Fatal("TLS/TCP fallback must not advertise datagram support")
	}
}
