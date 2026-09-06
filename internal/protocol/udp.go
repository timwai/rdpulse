package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// UDP protocol constants
const (
	UDPProtocolVersion = 1

	UDPHeaderSize = 12

	UDPTypeData = 0x11

	// MaxUDPPayload is the conservative payload limit to keep Datagram under typical MTUs
	DefaultMaxUDPPayload = 1150

	// MaxUDPFragments allowed for reassembly
	MaxUDPFragments = 64

	// MaxUDPPacketSize is the maximum size of an unfragmented UDP packet (64 KiB)
	MaxUDPPacketSize = 65535
)

var (
	ErrUDPDataTooShort       = errors.New("udp packet too short for header")
	ErrUDPUnsupportedVersion = errors.New("unsupported udp protocol version")
	ErrUDPUnsupportedType    = errors.New("unsupported udp packet type")
	ErrUDPInvalidFragment    = errors.New("invalid udp fragment metadata")
)

// UDPHeader represents the 12-byte fixed datagram header
type UDPHeader struct {
	Version       uint8
	Type          uint8
	SessionID     uint32
	PacketID      uint32
	FragmentID    uint8
	FragmentCount uint8
}

// EncodeUDP encodes the header and payload into dst slice without reflection or extra allocations.
// If cap(dst) is large enough, dst will be sliced and returned; otherwise a new slice of exact length is allocated.
func EncodeUDP(dst []byte, sessionID uint32, packetID uint32, fragID uint8, fragCount uint8, payload []byte) []byte {
	totalLen := UDPHeaderSize + len(payload)
	if cap(dst) >= totalLen {
		dst = dst[:totalLen]
	} else {
		dst = make([]byte, totalLen)
	}

	dst[0] = UDPProtocolVersion
	dst[1] = UDPTypeData
	binary.BigEndian.PutUint32(dst[2:6], sessionID)
	binary.BigEndian.PutUint32(dst[6:10], packetID)
	dst[10] = fragID
	dst[11] = fragCount

	copy(dst[UDPHeaderSize:], payload)
	return dst
}

// DecodeUDP parses the 12-byte fixed header and returns the header struct and payload slice (referencing data).
func DecodeUDP(data []byte) (UDPHeader, []byte, error) {
	if len(data) < UDPHeaderSize {
		return UDPHeader{}, nil, ErrUDPDataTooShort
	}

	version := data[0]
	if version != UDPProtocolVersion {
		return UDPHeader{}, nil, fmt.Errorf("%w: %d", ErrUDPUnsupportedVersion, version)
	}

	pktType := data[1]
	if pktType != UDPTypeData {
		return UDPHeader{}, nil, fmt.Errorf("%w: 0x%02x", ErrUDPUnsupportedType, pktType)
	}

	sessionID := binary.BigEndian.Uint32(data[2:6])
	packetID := binary.BigEndian.Uint32(data[6:10])
	fragID := data[10]
	fragCount := data[11]

	if fragCount == 0 || fragID >= fragCount || fragCount > MaxUDPFragments {
		return UDPHeader{}, nil, ErrUDPInvalidFragment
	}

	header := UDPHeader{
		Version:       version,
		Type:          pktType,
		SessionID:     sessionID,
		PacketID:      packetID,
		FragmentID:    fragID,
		FragmentCount: fragCount,
	}

	return header, data[UDPHeaderSize:], nil
}
