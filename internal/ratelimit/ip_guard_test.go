package ratelimit

import (
	"net"
	"testing"
	"time"
)

func TestIPGuardConcurrencyAndRate(t *testing.T) {
	guard := NewIPGuard(1, 2, time.Minute)
	addr := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1000}
	release, ok := guard.Acquire(addr)
	if !ok {
		t.Fatal("first acquire rejected")
	}
	if _, ok := guard.Acquire(&net.TCPAddr{IP: addr.IP, Port: 1001}); ok {
		t.Fatal("concurrent acquire was accepted")
	}
	release()
	if !guard.Allow(addr) {
		t.Fatal("second rate slot rejected")
	}
	if guard.Allow(addr) {
		t.Fatal("rate limit did not reject third attempt")
	}
}
