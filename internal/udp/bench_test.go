package udp

import (
	"testing"
	"time"

	"rdpulse/internal/protocol"
)

// BenchmarkReassembleSingleFragment models the common case: an RDP UDP packet
// that fits in one datagram.
func BenchmarkReassembleSingleFragment(b *testing.B) {
	r := NewReassembler(100 * time.Millisecond)
	payload := make([]byte, protocol.DefaultMaxUDPPayload)
	dst := make([]byte, 0, protocol.MaxUDPPacketSize)
	hdr := protocol.UDPHeader{SessionID: 1, FragmentID: 0, FragmentCount: 1}

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hdr.PacketID = uint32(i)
		if _, err := r.FeedAppend(dst[:0], &hdr, payload); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReassembleFourFragments models a large frame update that has to be
// split across datagrams.
func BenchmarkReassembleFourFragments(b *testing.B) {
	r := NewReassembler(time.Second)
	fragmenter := NewFragmenter(protocol.DefaultMaxUDPPayload)
	packet := make([]byte, 4*protocol.DefaultMaxUDPPayload)

	var datagrams [][]byte
	if err := fragmenter.Fragment(1, 1, packet, func(datagram []byte) error {
		datagrams = append(datagrams, append([]byte(nil), datagram...))
		return nil
	}); err != nil {
		b.Fatal(err)
	}

	dst := make([]byte, 0, protocol.MaxUDPPacketSize)
	b.ReportAllocs()
	b.SetBytes(int64(len(packet)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, datagram := range datagrams {
			hdr, fragPayload, err := protocol.DecodeUDP(datagram)
			if err != nil {
				b.Fatal(err)
			}
			hdr.PacketID = uint32(i)
			if _, err := r.FeedAppend(dst[:0], &hdr, fragPayload); err != nil {
				b.Fatal(err)
			}
		}
	}
}
