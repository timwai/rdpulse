package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// TCP protocol constants
const (
	TCPProtocolVersion = 1

	TCPHeaderSize = 8

	TCPTypeOpen   = 0x01
	TCPTypeOpenOK = 0x02
	TCPTypeError  = 0x03
	TCPTypeClose  = 0x04
)

// TCPHeader is the 8-byte initial binary header sent when a QUIC stream is opened for a TCP connection
type TCPHeader struct {
	Version      uint8
	Type         uint8
	ConnectionID uint32
	TargetPort   uint16
}

// Encode encodes the TCPHeader into an 8-byte slice
func (h *TCPHeader) Encode(b []byte) {
	if len(b) < TCPHeaderSize {
		panic("buffer too small for TCPHeader")
	}
	b[0] = h.Version
	b[1] = h.Type
	binary.BigEndian.PutUint32(b[2:6], h.ConnectionID)
	binary.BigEndian.PutUint16(b[6:8], h.TargetPort)
}

// Decode decodes an 8-byte slice into TCPHeader
func (h *TCPHeader) Decode(b []byte) error {
	if len(b) < TCPHeaderSize {
		return errors.New("insufficient data for TCPHeader")
	}
	h.Version = b[0]
	h.Type = b[1]
	h.ConnectionID = binary.BigEndian.Uint32(b[2:6])
	h.TargetPort = binary.BigEndian.Uint16(b[6:8])
	return nil
}

// ReadTCPHeader reads a TCPHeader from r
func ReadTCPHeader(r io.Reader) (*TCPHeader, error) {
	var buf [TCPHeaderSize]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, err
	}
	var h TCPHeader
	if err := h.Decode(buf[:]); err != nil {
		return nil, err
	}
	if h.Version != TCPProtocolVersion {
		return nil, fmt.Errorf("unsupported TCP protocol version: %d", h.Version)
	}
	return &h, nil
}

// WriteTCPHeader writes a TCPHeader to w
func WriteTCPHeader(w io.Writer, h *TCPHeader) error {
	var buf [TCPHeaderSize]byte
	h.Encode(buf[:])
	_, err := w.Write(buf[:])
	return err
}
