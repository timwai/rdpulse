package agent

import (
	"time"

	"rdpulse/internal/protocol"
)

const (
	p2pUDPReadSlice    = 500 * time.Millisecond
	p2pUDPIdleTimeout  = 45 * time.Second
	p2pUDPKeepaliveInt = 15 * time.Second
)

func p2pUDPIdleLimit() int {
	return int(p2pUDPIdleTimeout / p2pUDPReadSlice)
}

func p2pUDPResetsIdle(n int, buf []byte) bool {
	if n <= 0 || len(buf) < 4 {
		return false
	}
	return n >= protocol.PunchPacketSize && string(buf[0:4]) == "RDPU"
}
