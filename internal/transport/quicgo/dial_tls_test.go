package quicgo

import (
	"crypto/tls"
	"testing"
)

func TestDialTLSConfigUsesHostnameNotResolvedIP(t *testing.T) {
	cfg, err := dialTLSConfig("www.tingjusting.fun:443", &tls.Config{NextProtos: []string{"rdpulse-quic"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerName != "www.tingjusting.fun" {
		t.Fatalf("ServerName = %q, want hostname so certificate is not checked against a resolved IP", cfg.ServerName)
	}
	if cfg.NextProtos[0] != "rdpulse-quic" {
		t.Fatalf("NextProtos mutated: %v", cfg.NextProtos)
	}
}

func TestDialTLSConfigKeepsExplicitServerName(t *testing.T) {
	cfg, err := dialTLSConfig("www.tingjusting.fun:443", &tls.Config{ServerName: "relay.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerName != "relay.example.com" {
		t.Fatalf("ServerName = %q, want explicit override", cfg.ServerName)
	}
}

func TestDialTLSConfigDoesNotUseBareIP(t *testing.T) {
	cfg, err := dialTLSConfig("119.4.52.123:443", &tls.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerName != "" {
		t.Fatalf("bare IP must not become ServerName, got %q", cfg.ServerName)
	}
}
