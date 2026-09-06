package protocol

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	PunchMagic = 0x52445055 // "RDPU" in ASCII

	PunchVersion = 1

	PunchTypePunch     = 0x01
	PunchTypePunchAck  = 0x02
	PunchTypeKeepalive = 0x03

	PunchPacketSize = 40
	PunchMACSize    = 16
)

var (
	ErrPunchTooShort   = errors.New("punch packet too short")
	ErrPunchBadMagic   = errors.New("invalid punch packet magic")
	ErrPunchBadVersion = errors.New("unsupported punch packet version")
	ErrPunchBadType    = errors.New("unsupported punch packet type")
	ErrPunchBadMAC     = errors.New("punch packet HMAC verification failed")
	ErrPunchMissingKey = errors.New("punch packet authentication key is required")
)

// PunchPacket represents a cryptographically verified UDP hole punch / keepalive packet
type PunchPacket struct {
	Magic     uint32
	Version   uint8
	Type      uint8
	SessionID uint64
	Nonce     uint64
	MAC       [PunchMACSize]byte
}

// ComputePunchMAC calculates HMAC-SHA256 over packet header fields using key and returns the first 16 bytes
func ComputePunchMAC(key []byte, magic uint32, version, pktType uint8, sessionID, nonce uint64) [PunchMACSize]byte {
	h := hmac.New(sha256.New, key)
	var buf [24]byte
	binary.BigEndian.PutUint32(buf[0:4], magic)
	buf[4] = version
	buf[5] = pktType
	buf[6] = 0 // reserved
	buf[7] = 0 // reserved
	binary.BigEndian.PutUint64(buf[8:16], sessionID)
	binary.BigEndian.PutUint64(buf[16:24], nonce)

	h.Write(buf[:])
	fullMAC := h.Sum(nil)

	var mac [PunchMACSize]byte
	copy(mac[:], fullMAC[:PunchMACSize])
	return mac
}

// NewPunchPacket creates and signs a new PunchPacket
func NewPunchPacket(pktType uint8, sessionID uint64, key []byte) (*PunchPacket, error) {
	if len(key) == 0 {
		return nil, ErrPunchMissingKey
	}
	var nonceBuf [8]byte
	if _, err := rand.Read(nonceBuf[:]); err != nil {
		return nil, err
	}
	nonce := binary.BigEndian.Uint64(nonceBuf[:])

	mac := ComputePunchMAC(key, PunchMagic, PunchVersion, pktType, sessionID, nonce)

	return &PunchPacket{
		Magic:     PunchMagic,
		Version:   PunchVersion,
		Type:      pktType,
		SessionID: sessionID,
		Nonce:     nonce,
		MAC:       mac,
	}, nil
}

// Encode serializes the PunchPacket into exactly 40 bytes
func (p *PunchPacket) Encode(dst []byte) []byte {
	if cap(dst) >= PunchPacketSize {
		dst = dst[:PunchPacketSize]
	} else {
		dst = make([]byte, PunchPacketSize)
	}

	binary.BigEndian.PutUint32(dst[0:4], p.Magic)
	dst[4] = p.Version
	dst[5] = p.Type
	dst[6] = 0 // reserved
	dst[7] = 0 // reserved
	binary.BigEndian.PutUint64(dst[8:16], p.SessionID)
	binary.BigEndian.PutUint64(dst[16:24], p.Nonce)
	copy(dst[24:40], p.MAC[:])
	return dst
}

// DecodePunchPacket deserializes and verifies HMAC of a 40-byte punch packet
func DecodePunchPacket(data []byte, key []byte) (*PunchPacket, error) {
	if len(key) == 0 {
		return nil, ErrPunchMissingKey
	}
	if len(data) < PunchPacketSize {
		return nil, ErrPunchTooShort
	}

	magic := binary.BigEndian.Uint32(data[0:4])
	if magic != PunchMagic {
		return nil, ErrPunchBadMagic
	}

	version := data[4]
	if version != PunchVersion {
		return nil, fmt.Errorf("%w: %d", ErrPunchBadVersion, version)
	}

	pktType := data[5]
	if pktType != PunchTypePunch && pktType != PunchTypePunchAck && pktType != PunchTypeKeepalive {
		return nil, fmt.Errorf("%w: 0x%02x", ErrPunchBadType, pktType)
	}

	sessionID := binary.BigEndian.Uint64(data[8:16])
	nonce := binary.BigEndian.Uint64(data[16:24])

	var receivedMAC [PunchMACSize]byte
	copy(receivedMAC[:], data[24:40])

	expectedMAC := ComputePunchMAC(key, magic, version, pktType, sessionID, nonce)
	if !hmac.Equal(receivedMAC[:], expectedMAC[:]) {
		return nil, ErrPunchBadMAC
	}

	return &PunchPacket{
		Magic:     magic,
		Version:   version,
		Type:      pktType,
		SessionID: sessionID,
		Nonce:     nonce,
		MAC:       receivedMAC,
	}, nil
}
