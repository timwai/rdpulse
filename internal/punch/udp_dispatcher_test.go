package punch

import (
	"net"
	"testing"
	"time"

	"rdpulse/internal/protocol"
)

func TestUDPDispatcherRoutesConcurrentSessions(t *testing.T) {
	receiver, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := NewUDPDispatcher(receiver)
	defer dispatcher.Close()
	key := []byte("dispatcher-test-token-at-least-32-bytes")
	one, releaseOne, err := dispatcher.Register(101, key)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOne()
	two, releaseTwo, err := dispatcher.Register(202, key)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseTwo()

	sender, err := net.DialUDP("udp", nil, receiver.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	for _, id := range []uint64{202, 101} {
		packet, err := protocol.NewPunchPacket(protocol.PunchTypePunch, id, key)
		if err != nil {
			t.Fatal(err)
		}
		var buf [protocol.PunchPacketSize]byte
		if _, err := sender.Write(packet.Encode(buf[:0])); err != nil {
			t.Fatal(err)
		}
	}

	for id, channel := range map[uint64]<-chan UDPPacket{101: one, 202: two} {
		select {
		case packet := <-channel:
			decoded, err := protocol.DecodePunchPacket(packet.Data, key)
			if err != nil || decoded.SessionID != id {
				t.Fatalf("session %d received wrong packet: decoded=%+v err=%v", id, decoded, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("session %d did not receive packet", id)
		}
	}
}
