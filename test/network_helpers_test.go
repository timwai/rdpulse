package test

import (
	"fmt"
	"net"
)

func listenTCPAndUDP() (net.Listener, *net.UDPConn, string, error) {
	for i := 0; i < 100; i++ {
		udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
		if err != nil {
			return nil, nil, "", err
		}
		port := udpConn.LocalAddr().(*net.UDPAddr).Port
		address := fmt.Sprintf("127.0.0.1:%d", port)
		tcpListener, err := net.Listen("tcp", address)
		if err == nil {
			return tcpListener, udpConn, address, nil
		}
		_ = udpConn.Close()
	}
	return nil, nil, "", fmt.Errorf("could not allocate a port available for both TCP and UDP")
}
