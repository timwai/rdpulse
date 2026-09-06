package nat

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"time"

	"rdpulse/internal/protocol"
)

const (
	CandidateTypeLAN       = "lan"
	CandidateTypeReflexive = "reflexive"

	ProtocolUDP = "udp"
	ProtocolTCP = "tcp"

	PriorityLANDirect = 1000
	PriorityUDPP2P    = 900
	PriorityTCPP2P    = 800
	PriorityQUICRelay = 500
	PriorityTLSRelay  = 100

	RendezvousMagic = 0x52445A56 // "RDZV" in ASCII
)

// DiscoverLANCandidates gathers all non-loopback, non-link-local IP addresses on the machine
func DiscoverLANCandidates(udpPort, tcpPort int) ([]protocol.CandidateInfo, error) {
	var candidates []protocol.CandidateInfo

	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list network interfaces failed: %w", err)
	}

	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}

			// Focus on IPv4 for LAN stability in V1/V2 baseline
			if ip4 := ip.To4(); ip4 != nil {
				if udpPort > 0 {
					candidates = append(candidates, protocol.CandidateInfo{
						Protocol: ProtocolUDP,
						Type:     CandidateTypeLAN,
						Address:  fmt.Sprintf("%s:%d", ip4.String(), udpPort),
						Priority: PriorityLANDirect,
					})
				}
				if tcpPort > 0 {
					candidates = append(candidates, protocol.CandidateInfo{
						Protocol: ProtocolTCP,
						Type:     CandidateTypeLAN,
						Address:  fmt.Sprintf("%s:%d", ip4.String(), tcpPort),
						Priority: PriorityLANDirect,
					})
				}
			}
		}
	}

	return candidates, nil
}

// ProbeReflexiveCandidate sends a Rendezvous probe packet over an existing or local UDP socket to discover external reflexive addr
func ProbeReflexiveCandidate(ctx context.Context, rendezvousAddr string, localConn *net.UDPConn) (*protocol.CandidateInfo, error) {
	if rendezvousAddr == "" {
		return nil, fmt.Errorf("rendezvous address is empty")
	}
	rAddr, err := net.ResolveUDPAddr("udp", rendezvousAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve rendezvous addr failed: %w", err)
	}

	var conn *net.UDPConn
	var shouldClose bool
	if localConn != nil {
		conn = localConn
	} else {
		c, err := net.ListenUDP("udp", nil)
		if err != nil {
			return nil, fmt.Errorf("listen udp failed: %w", err)
		}
		conn = c
		shouldClose = true
	}

	if shouldClose {
		defer conn.Close()
	}
	defer conn.SetReadDeadline(time.Time{})

	// Probe packet: 4 bytes Magic (0x52445A56) + 1 byte Type (0x01 = Probe)
	probePkt := [5]byte{}
	binary.BigEndian.PutUint32(probePkt[0:4], RendezvousMagic)
	probePkt[4] = 0x01

	if _, err := conn.WriteToUDP(probePkt[:], rAddr); err != nil {
		return nil, fmt.Errorf("send rendezvous probe failed: %w", err)
	}

	// Read reply with timeout
	deadline := time.Now().Add(2 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetReadDeadline(deadline)
	respBuf := make([]byte, 64)
	n, responder, err := conn.ReadFromUDP(respBuf)
	if err != nil {
		return nil, fmt.Errorf("read rendezvous response failed: %w", err)
	}
	if responder == nil || responder.Port != rAddr.Port || !responder.IP.Equal(rAddr.IP) {
		return nil, fmt.Errorf("rendezvous response from unexpected source %v", responder)
	}

	// Expected response: 4 bytes Magic + 1 byte Type (0x02 = ProbeResp) + 4 bytes IPv4 + 2 bytes Port
	if n < 11 {
		return nil, fmt.Errorf("rendezvous response too short: %d bytes", n)
	}

	magic := binary.BigEndian.Uint32(respBuf[0:4])
	if magic != RendezvousMagic || respBuf[4] != 0x02 {
		return nil, fmt.Errorf("invalid rendezvous response header")
	}

	ipBytes := respBuf[5:9]
	port := binary.BigEndian.Uint16(respBuf[9:11])
	extAddr := netip.AddrPortFrom(netip.AddrFrom4([4]byte(ipBytes)), port)

	return &protocol.CandidateInfo{
		Protocol: ProtocolUDP,
		Type:     CandidateTypeReflexive,
		Address:  extAddr.String(),
		Priority: PriorityUDPP2P,
	}, nil
}
