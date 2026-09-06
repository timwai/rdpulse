package udp

import (
	"bytes"
	"context"
	"crypto/rand"
	"net"
	"net/netip"
	"testing"
	"time"

	"rdpulse/internal/protocol"
)

func TestFragmenterAndReassembly(t *testing.T) {
	fragmenter := NewFragmenter(100) // small payload limit for test
	reassembler := NewReassembler(200 * time.Millisecond)

	// Create random 350-byte packet
	packet := make([]byte, 350)
	_, _ = rand.Read(packet)

	sessionID := uint32(1001)
	packetID := uint32(2002)

	var fragments [][]byte
	err := fragmenter.Fragment(sessionID, packetID, packet, func(datagram []byte) error {
		fragCopy := make([]byte, len(datagram))
		copy(fragCopy, datagram)
		fragments = append(fragments, fragCopy)
		return nil
	})
	if err != nil {
		t.Fatalf("Fragment failed: %v", err)
	}

	// 350 / 100 -> 4 fragments
	if len(fragments) != 4 {
		t.Fatalf("expected 4 fragments, got %d", len(fragments))
	}

	// Feed in out-of-order: 3, 1, 0, 2
	order := []int{3, 1, 0, 2}
	var assembled []byte
	for _, idx := range order {
		hdr, payload, decErr := protocol.DecodeUDP(fragments[idx])
		if decErr != nil {
			t.Fatalf("DecodeUDP failed on fragment %d: %v", idx, decErr)
		}
		res, feedErr := reassembler.Feed(&hdr, payload)
		if feedErr != nil {
			t.Fatalf("Feed failed on fragment %d: %v", idx, feedErr)
		}
		if res != nil {
			assembled = res
		}
	}

	if assembled == nil {
		t.Fatal("expected packet to be fully assembled")
	}

	if !bytes.Equal(assembled, packet) {
		t.Fatal("assembled packet does not match original packet")
	}
}

func TestReassemblyTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	timeout := 50 * time.Millisecond
	reassembler := NewReassembler(timeout)
	reassembler.StartCleaner(ctx, 20*time.Millisecond)

	hdr := &protocol.UDPHeader{
		Version:       protocol.UDPProtocolVersion,
		Type:          protocol.UDPTypeData,
		SessionID:     101,
		PacketID:      202,
		FragmentID:    0,
		FragmentCount: 3, // Requires 3 fragments, but only 1 provided
	}

	_, err := reassembler.Feed(hdr, []byte("part 1"))
	if err != nil {
		t.Fatalf("Feed failed: %v", err)
	}

	// Wait past timeout
	time.Sleep(100 * time.Millisecond)

	reassembler.mu.Lock()
	count := len(reassembler.pending)
	reassembler.mu.Unlock()

	if count != 0 {
		t.Fatalf("expected timed out reassembly entry to be cleaned, still got %d entries", count)
	}
}

func TestSessionManager(t *testing.T) {
	sm := NewSessionManager(50*time.Millisecond, nil)

	addr := netip.MustParseAddrPort("1.2.3.4:5678")
	key := SessionKey{
		DeviceID:   "dev-1",
		PublicPort: 20001,
		ClientAddr: addr,
	}

	sess, created := sm.GetOrCreateByKey(key)
	if !created {
		t.Fatal("expected session to be created")
	}

	// Lookup by ID
	foundByID := sm.GetByID(sess.ID)
	if foundByID == nil || foundByID.ID != sess.ID {
		t.Fatalf("GetByID failed: got %+v", foundByID)
	}

	// Lookup by Key
	foundByKey := sm.GetByKey(key)
	if foundByKey == nil || foundByKey.ID != sess.ID {
		t.Fatalf("GetByKey failed: got %+v", foundByKey)
	}

	// Test sweeper
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sm.StartSweeper(ctx, 20*time.Millisecond)

	time.Sleep(100 * time.Millisecond)
	if count := sm.ActiveCount(); count != 0 {
		t.Fatalf("expected session to expire and be cleaned, active count: %d", count)
	}
}

func TestSessionManagerLimits(t *testing.T) {
	sm := NewSessionManagerWithLimits(time.Minute, nil, SessionLimits{
		MaxSessions:        2,
		MaxSessionsPerIP:   1,
		MaxCreatePerSecond: 10,
	})
	defer sm.Close()

	firstKey := SessionKey{ClientAddr: netip.MustParseAddrPort("192.0.2.1:1000")}
	if sess, created := sm.GetOrCreateByKey(firstKey); sess == nil || !created {
		t.Fatal("expected first session to be created")
	}
	if sess, _ := sm.GetOrCreateByKey(SessionKey{ClientAddr: netip.MustParseAddrPort("192.0.2.1:1001")}); sess != nil {
		t.Fatal("expected per-IP limit to reject second session")
	}
	if sess, created := sm.GetOrCreateByKey(SessionKey{ClientAddr: netip.MustParseAddrPort("192.0.2.2:1000")}); sess == nil || !created {
		t.Fatal("expected session from second IP to be created")
	}
	if sess, _ := sm.GetOrCreateByKey(SessionKey{ClientAddr: netip.MustParseAddrPort("192.0.2.3:1000")}); sess != nil {
		t.Fatal("expected global session limit to reject third session")
	}
}

func TestSessionManagerCreateRateLimit(t *testing.T) {
	sm := NewSessionManagerWithLimits(time.Minute, nil, SessionLimits{
		MaxSessions:        10,
		MaxSessionsPerIP:   10,
		MaxCreatePerSecond: 1,
	})
	defer sm.Close()

	if sess, _ := sm.GetOrCreateByKey(SessionKey{ClientAddr: netip.MustParseAddrPort("192.0.2.1:1000")}); sess == nil {
		t.Fatal("expected first session to be created")
	}
	if sess, _ := sm.GetOrCreateByKey(SessionKey{ClientAddr: netip.MustParseAddrPort("192.0.2.2:1000")}); sess != nil {
		t.Fatal("expected session creation rate limit to reject second session")
	}
}

func TestSessionManagerCloseUnblocksUDPRead(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}

	sm := NewSessionManager(time.Minute, nil)
	if sess := sm.AddSessionWithID(1, conn); sess == nil {
		t.Fatal("expected session to be added")
	}

	readDone := make(chan error, 1)
	go func() {
		var buf [1]byte
		_, _, readErr := conn.ReadFromUDP(buf[:])
		readDone <- readErr
	}()

	if err := sm.Close(); err != nil {
		t.Fatalf("close manager: %v", err)
	}
	if got := sm.ActiveCount(); got != 0 {
		t.Fatalf("active sessions after close: %d", got)
	}

	select {
	case readErr := <-readDone:
		if readErr == nil {
			t.Fatal("expected UDP read to fail after close")
		}
	case <-time.After(time.Second):
		t.Fatal("UDP read remained blocked after manager close")
	}
}
