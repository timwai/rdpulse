package agent

import (
	"testing"
	"time"

	"rdpulse/internal/protocol"
)

func TestP2PUDPIdleLimitOutlastsKeepalive(t *testing.T) {
	idle := time.Duration(p2pUDPIdleLimit()) * p2pUDPReadSlice
	if idle < p2pUDPKeepaliveInt*2 {
		t.Fatalf("P2P UDP idle %s is shorter than two keepalives (%s)", idle, p2pUDPKeepaliveInt)
	}
}

func TestP2PUDPKeepaliveCountsAsActivity(t *testing.T) {
	buf := make([]byte, protocol.PunchPacketSize)
	copy(buf, "RDPU")
	if !p2pUDPResetsIdle(len(buf), buf) {
		t.Fatal("keepalive packet should reset P2P UDP idle timer")
	}
	if p2pUDPResetsIdle(0, nil) {
		t.Fatal("empty read should not reset idle timer")
	}
}
