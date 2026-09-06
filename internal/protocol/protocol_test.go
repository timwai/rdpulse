package protocol

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

func TestControlMessageReadWrite(t *testing.T) {
	msg := &ControlMessage{
		Type:               MsgTypeRegister,
		DeviceID:           "test-pc-001",
		Hostname:           "DESKTOP-WIN",
		AgentVersion:       "1.0.0",
		MinProtocolVersion: 1,
		MaxProtocolVersion: 1,
	}

	var buf bytes.Buffer
	if err := WriteControlMessage(&buf, msg); err != nil {
		t.Fatalf("WriteControlMessage failed: %v", err)
	}

	decoded, err := ReadControlMessage(&buf)
	if err != nil {
		t.Fatalf("ReadControlMessage failed: %v", err)
	}

	if decoded.Type != msg.Type || decoded.DeviceID != msg.DeviceID || decoded.Hostname != msg.Hostname {
		t.Fatalf("mismatch in decoded control message: %+v vs %+v", decoded, msg)
	}
}

func TestControlWriterConcurrentFrames(t *testing.T) {
	const messageCount = 200

	var buf bytes.Buffer
	writer := NewControlWriter(&buf)
	errCh := make(chan error, messageCount)

	var wg sync.WaitGroup
	for i := 0; i < messageCount; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			errCh <- writer.WriteMessage(&ControlMessage{
				Type:     MsgTypePing,
				DeviceID: fmt.Sprintf("device-%03d", id),
			})
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent control write failed: %v", err)
		}
	}

	seen := make(map[string]bool, messageCount)
	for i := 0; i < messageCount; i++ {
		msg, err := ReadControlMessage(&buf)
		if err != nil {
			t.Fatalf("read concurrent control frame %d failed: %v", i, err)
		}
		seen[msg.DeviceID] = true
	}
	if len(seen) != messageCount {
		t.Fatalf("got %d unique messages, want %d", len(seen), messageCount)
	}
	if buf.Len() != 0 {
		t.Fatalf("unexpected trailing control bytes: %d", buf.Len())
	}
}

func TestTCPHeaderEncodeDecode(t *testing.T) {
	hdr := &TCPHeader{
		Version:      TCPProtocolVersion,
		Type:         TCPTypeOpen,
		ConnectionID: 10086,
		TargetPort:   3389,
	}

	var buf [TCPHeaderSize]byte
	hdr.Encode(buf[:])

	var decoded TCPHeader
	if err := decoded.Decode(buf[:]); err != nil {
		t.Fatalf("TCPHeader Decode failed: %v", err)
	}

	if decoded != *hdr {
		t.Fatalf("mismatch TCPHeader: got %+v, want %+v", decoded, *hdr)
	}

	var streamBuf bytes.Buffer
	if err := WriteTCPHeader(&streamBuf, hdr); err != nil {
		t.Fatalf("WriteTCPHeader failed: %v", err)
	}

	readHdr, err := ReadTCPHeader(&streamBuf)
	if err != nil {
		t.Fatalf("ReadTCPHeader failed: %v", err)
	}
	if *readHdr != *hdr {
		t.Fatalf("mismatch ReadTCPHeader: got %+v, want %+v", *readHdr, *hdr)
	}
}

func TestUDPDatagramEncodeDecode(t *testing.T) {
	payload := []byte("hello rdp udp datagram")
	sessionID := uint32(0x12345678)
	packetID := uint32(99999)
	fragID := uint8(0)
	fragCount := uint8(1)

	var dst [64]byte
	encoded := EncodeUDP(dst[:0], sessionID, packetID, fragID, fragCount, payload)

	if len(encoded) != UDPHeaderSize+len(payload) {
		t.Fatalf("unexpected encoded length: %d", len(encoded))
	}

	header, decodedPayload, err := DecodeUDP(encoded)
	if err != nil {
		t.Fatalf("DecodeUDP failed: %v", err)
	}

	if header.Version != UDPProtocolVersion || header.Type != UDPTypeData ||
		header.SessionID != sessionID || header.PacketID != packetID ||
		header.FragmentID != fragID || header.FragmentCount != fragCount {
		t.Fatalf("header mismatch: %+v", header)
	}

	if !bytes.Equal(decodedPayload, payload) {
		t.Fatalf("payload mismatch: %s vs %s", string(decodedPayload), string(payload))
	}

	// Test invalid fragment count
	badData := make([]byte, len(encoded))
	copy(badData, encoded)
	badData[11] = 0 // fragment count = 0
	if _, _, err := DecodeUDP(badData); err == nil {
		t.Fatal("expected error for fragCount = 0")
	}

	// Test fragmentID >= fragmentCount
	copy(badData, encoded)
	badData[10] = 2 // fragID
	badData[11] = 2 // fragCount
	if _, _, err := DecodeUDP(badData); err == nil {
		t.Fatal("expected error for fragID >= fragCount")
	}
}

func TestP2PDataAuthentication(t *testing.T) {
	key := []byte("session-token-at-least-32-bytes")
	sessionID := uint32(1234)
	datagram := EncodeUDP(nil, sessionID, 9, 0, 1, []byte("authenticated payload"))

	packet, err := EncodeP2PData(sessionID, key, datagram)
	if err != nil {
		t.Fatalf("encode P2P data: %v", err)
	}
	decoded, err := DecodeP2PData(packet, sessionID, key)
	if err != nil {
		t.Fatalf("decode P2P data: %v", err)
	}
	if !bytes.Equal(decoded, datagram) {
		t.Fatal("decoded P2P datagram does not match input")
	}

	tampered := append([]byte(nil), packet...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := DecodeP2PData(tampered, sessionID, key); err == nil {
		t.Fatal("expected tampered P2P data to be rejected")
	}
	if _, err := DecodeP2PData(packet, sessionID, []byte("wrong-token")); err == nil {
		t.Fatal("expected wrong P2P token to be rejected")
	}
	if _, err := DecodeP2PData(packet, sessionID+1, key); err == nil {
		t.Fatal("expected wrong P2P session ID to be rejected")
	}
	if _, err := DecodeP2PData(packet, sessionID, nil); err == nil {
		t.Fatal("expected missing P2P token to be rejected")
	}
}

func TestFormatCandidates(t *testing.T) {
	if got := FormatCandidates(nil); got != "无" {
		t.Fatalf("empty = %q", got)
	}
	got := FormatCandidates([]CandidateInfo{
		{Type: "lan", Protocol: "udp", Address: "192.168.1.8:40000"},
		{Type: "reflexive", Protocol: "tcp", Address: "203.0.113.9:443"},
	})
	want := "lan/udp 192.168.1.8:40000, reflexive/tcp 203.0.113.9:443"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
