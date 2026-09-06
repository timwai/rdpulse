package udp

import (
	"errors"
	"fmt"

	"rdpulse/internal/protocol"
)

var (
	ErrPacketTooLarge = errors.New("packet exceeds maximum allowed fragments")
)

// Fragmenter handles breaking large UDP packets into datagram fragments
type Fragmenter struct {
	maxPayload int
}

// NewFragmenter creates a Fragmenter with the specified maxPayload per datagram
func NewFragmenter(maxPayload int) *Fragmenter {
	if maxPayload <= 0 {
		maxPayload = protocol.DefaultMaxUDPPayload
	}
	return &Fragmenter{
		maxPayload: maxPayload,
	}
}

// Fragment splits packet and invokes handleFrag for each encoded datagram.
// This allows zero-allocation direct transmission into the transport without intermediate slice slice allocations.
func (f *Fragmenter) Fragment(sessionID uint32, packetID uint32, packet []byte, handleFrag func(datagram []byte) error) error {
	packetLen := len(packet)
	if packetLen == 0 {
		// Empty UDP packet
		bufPtr := GetBuffer()
		defer PutBuffer(bufPtr)
		encoded := protocol.EncodeUDP((*bufPtr)[:0], sessionID, packetID, 0, 1, nil)
		return handleFrag(encoded)
	}

	numFrags := (packetLen + f.maxPayload - 1) / f.maxPayload
	if numFrags > protocol.MaxUDPFragments {
		return fmt.Errorf("%w: fragments %d > %d", ErrPacketTooLarge, numFrags, protocol.MaxUDPFragments)
	}

	fragCount := uint8(numFrags)
	bufPtr := GetBuffer()
	defer PutBuffer(bufPtr)

	for i := 0; i < numFrags; i++ {
		start := i * f.maxPayload
		end := start + f.maxPayload
		if end > packetLen {
			end = packetLen
		}
		chunk := packet[start:end]

		encoded := protocol.EncodeUDP((*bufPtr)[:0], sessionID, packetID, uint8(i), fragCount, chunk)
		if err := handleFrag(encoded); err != nil {
			return err
		}
	}

	return nil
}
