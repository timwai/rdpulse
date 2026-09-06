package protocol

import (
	"testing"
)

func TestPunchPacketSignAndVerify(t *testing.T) {
	key := []byte("session-secret-key-12345678")
	sessionID := uint64(0x1122334455667788)

	pkt, err := NewPunchPacket(PunchTypePunch, sessionID, key)
	if err != nil {
		t.Fatalf("NewPunchPacket failed: %v", err)
	}

	var buf [PunchPacketSize]byte
	encoded := pkt.Encode(buf[:0])

	if len(encoded) != PunchPacketSize {
		t.Fatalf("expected size %d, got %d", PunchPacketSize, len(encoded))
	}

	decoded, err := DecodePunchPacket(encoded, key)
	if err != nil {
		t.Fatalf("DecodePunchPacket failed: %v", err)
	}

	if decoded.Magic != PunchMagic || decoded.Type != PunchTypePunch || decoded.SessionID != sessionID || decoded.Nonce != pkt.Nonce {
		t.Fatalf("decoded packet mismatch: %+v vs %+v", decoded, pkt)
	}

	// Test tampering with payload (e.g. flip a bit in SessionID)
	corruptData := make([]byte, len(encoded))
	copy(corruptData, encoded)
	corruptData[8] ^= 0xFF

	if _, err := DecodePunchPacket(corruptData, key); err == nil {
		t.Fatal("expected HMAC verification failure on corrupted packet")
	}

	// Test wrong key
	wrongKey := []byte("wrong-key")
	if _, err := DecodePunchPacket(encoded, wrongKey); err == nil {
		t.Fatal("expected HMAC verification failure on wrong key")
	}
	if _, err := DecodePunchPacket(encoded, nil); err == nil {
		t.Fatal("expected missing authentication key to be rejected")
	}
}
