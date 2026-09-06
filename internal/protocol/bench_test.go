package protocol

import "testing"

var benchKey = []byte("p2p-session-token-at-least-32-bytes")

// BenchmarkP2PCodecEncode measures the cached-key authentication path used for
// every outgoing P2P datagram.
func BenchmarkP2PCodecEncode(b *testing.B) {
	codec, err := NewP2PCodec(42, benchKey)
	if err != nil {
		b.Fatal(err)
	}
	datagram := make([]byte, UDPHeaderSize+DefaultMaxUDPPayload)
	dst := make([]byte, 0, MaxUDPPacketSize+P2PDataHeaderSize)

	b.ReportAllocs()
	b.SetBytes(int64(len(datagram)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		codec.Encode(dst[:0], datagram)
	}
}

// BenchmarkEncodeP2PDataPerPacketKey is the previous behaviour, where every
// datagram rebuilt the HMAC key. Kept as the baseline to compare against.
func BenchmarkEncodeP2PDataPerPacketKey(b *testing.B) {
	datagram := make([]byte, UDPHeaderSize+DefaultMaxUDPPayload)

	b.ReportAllocs()
	b.SetBytes(int64(len(datagram)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := EncodeP2PData(42, benchKey, datagram); err != nil {
			b.Fatal(err)
		}
	}
}
