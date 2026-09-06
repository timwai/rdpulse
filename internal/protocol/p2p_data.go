package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"sync"
)

const (
	P2PDataMagic      = 0x52445044 // "RDPD"
	P2PDataMACSize    = 16
	P2PDataHeaderSize = 4 + 4 + P2PDataMACSize
)

var (
	ErrP2PDataTooShort   = errors.New("p2p data packet too short")
	ErrP2PDataBadMagic   = errors.New("invalid p2p data packet magic")
	ErrP2PDataBadSession = errors.New("unexpected p2p data session")
	ErrP2PDataMissingKey = errors.New("p2p data authentication key is required")
	ErrP2PDataBadMAC     = errors.New("p2p data HMAC verification failed")
)

// P2PCodec authenticates direct P2P datagrams for one session. It caches the
// expanded HMAC key so the per-datagram path neither allocates nor repeats key
// setup. It is safe for concurrent use.
type P2PCodec struct {
	sessionID uint32

	mu     sync.Mutex
	mac    hash.Hash
	digest [sha256.Size]byte
}

// NewP2PCodec builds a codec bound to one session ID and session token.
func NewP2PCodec(sessionID uint32, key []byte) (*P2PCodec, error) {
	if len(key) == 0 {
		return nil, ErrP2PDataMissingKey
	}
	return &P2PCodec{
		sessionID: sessionID,
		mac:       hmac.New(sha256.New, key),
	}, nil
}

// Encode authenticates one already-framed UDP datagram into dst. When cap(dst)
// is large enough the packet is built in place. The result never aliases
// datagram.
func (c *P2PCodec) Encode(dst, datagram []byte) []byte {
	totalLen := P2PDataHeaderSize + len(datagram)
	if cap(dst) >= totalLen {
		dst = dst[:totalLen]
	} else {
		dst = make([]byte, totalLen)
	}

	binary.BigEndian.PutUint32(dst[0:4], P2PDataMagic)
	binary.BigEndian.PutUint32(dst[4:8], c.sessionID)
	copy(dst[P2PDataHeaderSize:], datagram)

	c.mu.Lock()
	c.mac.Reset()
	_, _ = c.mac.Write(dst[0:8])
	_, _ = c.mac.Write(dst[P2PDataHeaderSize:])
	sum := c.mac.Sum(c.digest[:0])
	copy(dst[8:P2PDataHeaderSize], sum[:P2PDataMACSize])
	c.mu.Unlock()

	return dst
}

// Decode verifies a direct P2P data packet and returns the enclosed UDP
// datagram. The returned slice aliases packet.
func (c *P2PCodec) Decode(packet []byte) ([]byte, error) {
	if len(packet) < P2PDataHeaderSize {
		return nil, ErrP2PDataTooShort
	}
	if binary.BigEndian.Uint32(packet[0:4]) != P2PDataMagic {
		return nil, ErrP2PDataBadMagic
	}
	if binary.BigEndian.Uint32(packet[4:8]) != c.sessionID {
		return nil, ErrP2PDataBadSession
	}

	datagram := packet[P2PDataHeaderSize:]

	c.mu.Lock()
	c.mac.Reset()
	_, _ = c.mac.Write(packet[0:8])
	_, _ = c.mac.Write(datagram)
	sum := c.mac.Sum(c.digest[:0])
	ok := hmac.Equal(packet[8:P2PDataHeaderSize], sum[:P2PDataMACSize])
	c.mu.Unlock()

	if !ok {
		return nil, ErrP2PDataBadMAC
	}
	return datagram, nil
}

// EncodeP2PData authenticates one already-framed UDP datagram for a direct
// peer-to-peer path. The returned slice does not alias datagram.
func EncodeP2PData(sessionID uint32, key, datagram []byte) ([]byte, error) {
	codec, err := NewP2PCodec(sessionID, key)
	if err != nil {
		return nil, err
	}
	return codec.Encode(nil, datagram), nil
}

// DecodeP2PData verifies a direct P2P data packet and returns the enclosed UDP
// datagram. The returned slice aliases packet.
func DecodeP2PData(packet []byte, expectedSessionID uint32, key []byte) ([]byte, error) {
	codec, err := NewP2PCodec(expectedSessionID, key)
	if err != nil {
		return nil, err
	}
	return codec.Decode(packet)
}
