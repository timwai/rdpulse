package agent

import (
	"context"
	"testing"
	"time"

	"rdpulse/internal/config"
	"rdpulse/internal/transport/quicgo"
	"rdpulse/internal/transport/tlsmux"
)

func TestDialRelayTransportAutomaticallyFallsBackToTLS(t *testing.T) {
	tlsConfig, err := quicgo.GenerateSelfSignedCert()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := tlsmux.Listen("127.0.0.1:0", tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	cfg := &config.AgentConfig{}
	cfg.Server.Address = listener.Addr().String() // UDP has no QUIC listener.
	cfg.Server.TLSAddress = listener.Addr().String()
	cfg.Server.InsecureSkipVerify = true
	cfg.Server.QUICDialTimeout = 100 * time.Millisecond
	client := NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tr, name, err := client.dialRelayTransport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if name != "TLS/TCP" {
		t.Fatalf("transport=%s, want TLS/TCP", name)
	}
	server, err := listener.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
}
