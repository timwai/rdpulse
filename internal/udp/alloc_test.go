package udp

import (
	"testing"
	"time"

	"rdpulse/internal/protocol"
)

func TestFeedAppendSingleFragmentIsAllocationFree(t *testing.T) {
	r := NewReassembler(100 * time.Millisecond)
	payload := make([]byte, protocol.DefaultMaxUDPPayload)
	dst := make([]byte, 0, protocol.MaxUDPPacketSize)

	var feedErr error
	var outLen int
	allocs := testing.AllocsPerRun(200, func() {
		hdr := protocol.UDPHeader{SessionID: 7, PacketID: 9, FragmentID: 0, FragmentCount: 1}
		out, err := r.FeedAppend(dst[:0], &hdr, payload)
		feedErr = err
		outLen = len(out)
	})
	if feedErr != nil {
		t.Fatalf("FeedAppend: %v", feedErr)
	}
	if outLen != len(payload) {
		t.Fatalf("assembled %d bytes, want %d", outLen, len(payload))
	}
	if allocs != 0 {
		t.Fatalf("single-fragment FeedAppend allocated %.0f times per packet, want 0", allocs)
	}
}

func TestFeedAppendMultiFragmentReusesPooledBuffers(t *testing.T) {
	r := NewReassembler(time.Second)
	fragmenter := NewFragmenter(protocol.DefaultMaxUDPPayload)
	packet := make([]byte, 4*protocol.DefaultMaxUDPPayload)
	for i := range packet {
		packet[i] = byte(i)
	}

	var datagrams [][]byte
	if err := fragmenter.Fragment(1, 1, packet, func(datagram []byte) error {
		datagrams = append(datagrams, append([]byte(nil), datagram...))
		return nil
	}); err != nil {
		t.Fatalf("Fragment: %v", err)
	}

	dst := make([]byte, 0, protocol.MaxUDPPacketSize)
	packetID := uint32(0)
	var assembledLen int
	var feedErr error

	allocs := testing.AllocsPerRun(100, func() {
		packetID++
		out := []byte(nil)
		for _, datagram := range datagrams {
			hdr, payload, err := protocol.DecodeUDP(datagram)
			if err != nil {
				feedErr = err
				return
			}
			hdr.PacketID = packetID
			assembled, err := r.FeedAppend(dst[:0], &hdr, payload)
			if err != nil {
				feedErr = err
				return
			}
			if assembled != nil {
				out = assembled
			}
		}
		assembledLen = len(out)
	})

	if feedErr != nil {
		t.Fatalf("FeedAppend: %v", feedErr)
	}
	if assembledLen != len(packet) {
		t.Fatalf("assembled %d bytes, want %d", assembledLen, len(packet))
	}
	if allocs > 1 {
		t.Fatalf("4-fragment reassembly allocated %.0f times per packet, want <= 1", allocs)
	}
}
