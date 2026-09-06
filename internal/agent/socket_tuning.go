package agent

import "net"

// udpSocketBufferBytes matches what Relay and Rendezvous already request. RDP
// arrives in bursts after a screen change, and an undersized kernel buffer
// turns those bursts into drops rather than into a slower stream.
const udpSocketBufferBytes = 2 * 1024 * 1024

// tuneUDPSocketBuffers widens the kernel receive and send buffers. Failures are
// ignored: the OS clamps to its own maximum and the socket stays usable.
func tuneUDPSocketBuffers(conn *net.UDPConn) {
	if conn == nil {
		return
	}
	_ = conn.SetReadBuffer(udpSocketBufferBytes)
	_ = conn.SetWriteBuffer(udpSocketBufferBytes)
}
