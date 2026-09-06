package protocol

import "testing"

func TestP2PCodecEncodeIsAllocationFree(t *testing.T) {
	codec, err := NewP2PCodec(42, []byte("p2p-session-token-at-least-32-bytes"))
	if err != nil {
		t.Fatalf("NewP2PCodec: %v", err)
	}
	datagram := make([]byte, UDPHeaderSize+DefaultMaxUDPPayload)
	dst := make([]byte, 0, MaxUDPPacketSize+P2PDataHeaderSize)

	var outLen int
	allocs := testing.AllocsPerRun(200, func() {
		outLen = len(codec.Encode(dst[:0], datagram))
	})
	if outLen != P2PDataHeaderSize+len(datagram) {
		t.Fatalf("encoded %d bytes, want %d", outLen, P2PDataHeaderSize+len(datagram))
	}
	if allocs != 0 {
		t.Fatalf("P2PCodec.Encode allocated %.0f times per datagram, want 0", allocs)
	}
}

func TestP2PCodecDecodeIsAllocationFree(t *testing.T) {
	codec, err := NewP2PCodec(42, []byte("p2p-session-token-at-least-32-bytes"))
	if err != nil {
		t.Fatalf("NewP2PCodec: %v", err)
	}
	packet := codec.Encode(nil, make([]byte, UDPHeaderSize+DefaultMaxUDPPayload))

	var decodeErr error
	allocs := testing.AllocsPerRun(200, func() {
		_, decodeErr = codec.Decode(packet)
	})
	if decodeErr != nil {
		t.Fatalf("Decode: %v", decodeErr)
	}
	if allocs != 0 {
		t.Fatalf("P2PCodec.Decode allocated %.0f times per datagram, want 0", allocs)
	}
}

func TestP2PCodecRoundTripsWithPackageHelpers(t *testing.T) {
	key := []byte("p2p-session-token-at-least-32-bytes")
	datagram := []byte("framed rdp udp datagram")

	codec, err := NewP2PCodec(4242, key)
	if err != nil {
		t.Fatalf("NewP2PCodec: %v", err)
	}
	packet := codec.Encode(make([]byte, 0, 256), datagram)

	decoded, err := DecodeP2PData(packet, 4242, key)
	if err != nil {
		t.Fatalf("DecodeP2PData: %v", err)
	}
	if string(decoded) != string(datagram) {
		t.Fatalf("decoded %q, want %q", decoded, datagram)
	}

	legacy, err := EncodeP2PData(4242, key, datagram)
	if err != nil {
		t.Fatalf("EncodeP2PData: %v", err)
	}
	if _, err := codec.Decode(legacy); err != nil {
		t.Fatalf("codec rejected packet from EncodeP2PData: %v", err)
	}
}
