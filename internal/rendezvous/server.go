package rendezvous

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"

	"rdpulse/internal/nat"
)

// Server handles UDP NAT mapping discovery (STUN-like functionality)
type Server struct {
	conn   *net.UDPConn
	closed bool
}

// StartServer starts the Rendezvous UDP server
func StartServer(ctx context.Context, listenAddr string) (*Server, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve rendezvous addr failed: %w", err)
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("listen rendezvous udp failed: %w", err)
	}
	_ = conn.SetReadBuffer(2 * 1024 * 1024)
	_ = conn.SetWriteBuffer(2 * 1024 * 1024)

	s := &Server{conn: conn}

	go s.serve(ctx)
	return s, nil
}

func (s *Server) Addr() net.Addr {
	return s.conn.LocalAddr()
}

func (s *Server) Close() error {
	s.closed = true
	return s.conn.Close()
}

func (s *Server) serve(ctx context.Context) {
	buf := make([]byte, 1024)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, remoteAddr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if s.closed {
				return
			}
			continue
		}

		// Verify minimum probe size (4 bytes magic + 1 byte type)
		if n < 5 {
			continue
		}

		magic := binary.BigEndian.Uint32(buf[0:4])
		pktType := buf[4]
		if magic != nat.RendezvousMagic || pktType != 0x01 {
			continue
		}

		// Prepare probe response: 4 bytes Magic + 1 byte Type (0x02) + 4 bytes IPv4 + 2 bytes Port (11 bytes total)
		ip4 := remoteAddr.IP.To4()
		if ip4 == nil {
			// Skip non-IPv4 for V1/V2 baseline
			continue
		}

		var resp [11]byte
		binary.BigEndian.PutUint32(resp[0:4], nat.RendezvousMagic)
		resp[4] = 0x02
		copy(resp[5:9], ip4)
		binary.BigEndian.PutUint16(resp[9:11], uint16(remoteAddr.Port))

		if _, err := s.conn.WriteToUDP(resp[:], remoteAddr); err != nil {
			log.Printf("[Rendezvous] Failed to send probe response to %s: %v", remoteAddr, err)
		}
	}
}
