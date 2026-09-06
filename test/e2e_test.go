package test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"rdpulse/internal/acl"
	"rdpulse/internal/agent"
	"rdpulse/internal/config"
	"rdpulse/internal/relay"
	"rdpulse/internal/storage"
	"rdpulse/internal/transport/quicgo"
)

func TestEndToEndRelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Start mock RDP server: bind TCP and UDP on the same port
	mockTCPListener, mockUDPConn, mockRDPAddr, err := listenTCPAndUDP()
	if err != nil {
		t.Fatalf("Failed to listen mock TCP/UDP: %v", err)
	}
	defer mockTCPListener.Close()
	defer mockUDPConn.Close()

	go func() {
		for {
			conn, err := mockTCPListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c) // Echo TCP data
			}(conn)
		}
	}()

	go func() {
		buf := make([]byte, 2048)
		for {
			n, remoteAddr, err := mockUDPConn.ReadFrom(buf)
			if err != nil {
				return
			}
			// Echo UDP packet back
			_, _ = mockUDPConn.WriteTo(buf[:n], remoteAddr)
		}
	}()

	// 2. Setup Relay
	dbPath := "test_relay.db"
	defer os.Remove(dbPath)
	defer os.Remove(dbPath + "-wal")
	defer os.Remove(dbPath + "-shm")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open storage: %v", err)
	}
	defer db.Close()

	portMgr, err := relay.NewPortManager(31000, 31100, db)
	if err != nil {
		t.Fatalf("Failed to create port manager: %v", err)
	}

	aclMgr, err := acl.NewManager("allow", nil)
	if err != nil {
		t.Fatalf("Failed to create ACL manager: %v", err)
	}

	relayCfg := &config.RelayConfig{}
	relayCfg.UDP.DatagramPayload = 1150
	relayCfg.UDP.SessionIdleTimeout = 5 * time.Second
	relayCfg.UDP.ReassemblyTimeout = 100 * time.Millisecond
	relayCfg.UDP.MaxFragments = 64
	relayCfg.RDP.PublicHost = "127.0.0.1"
	enrollmentToken := "test-enrollment-token-at-least-32-bytes"
	relayCfg.Security.EnrollmentInvitations = []config.EnrollmentInvitation{
		{DeviceID: "test-agent-001", Token: enrollmentToken, ExpiresAt: time.Now().Add(time.Hour)},
	}

	agentMgr := relay.NewAgentManager(relayCfg, db, portMgr, aclMgr)
	defer agentMgr.Close()

	tlsConf, err := quicgo.GenerateSelfSignedCert()
	if err != nil {
		t.Fatalf("Failed to generate tls cert: %v", err)
	}

	quicListener, err := quicgo.ListenAddr("127.0.0.1:0", tlsConf, quicgo.DefaultQUICConfig())
	if err != nil {
		t.Fatalf("Failed to listen QUIC: %v", err)
	}
	defer quicListener.Close()

	quicServerAddr := quicListener.Addr().String()

	go func() {
		for {
			tr, err := quicListener.Accept(ctx)
			if err != nil {
				return
			}
			go agentMgr.HandleIncomingConnection(ctx, tr)
		}
	}()

	// 3. Setup Agent pointing to mock RDP address (both TCP & UDP)
	agentCfg := &config.AgentConfig{}
	agentCfg.Server.Address = quicServerAddr
	agentCfg.Server.InsecureSkipVerify = true
	agentCfg.Device.ID = "test-agent-001"
	agentCfg.Device.Secret = "mock-secret-key-at-least-32-bytes"
	agentCfg.Device.EnrollmentToken = enrollmentToken
	agentCfg.RDP.Address = mockRDPAddr
	agentCfg.Transport.DatagramPayload = 1150
	agentCfg.UDP.SessionIdleTimeout = 5 * time.Second
	agentCfg.UDP.ReassemblyTimeout = 100 * time.Millisecond
	agentCfg.UDP.MaxFragments = 64
	agentCfg.Heartbeat.Interval = 2 * time.Second
	agentCfg.Reconnect.MaxInterval = 5 * time.Second

	agentClient := agent.NewClient(agentCfg)

	go func() {
		_ = agentClient.Run(ctx)
	}()

	// Wait for agent to connect and port to be assigned
	var assignedPort uint16
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		sess := agentMgr.GetAgentByDeviceID("test-agent-001")
		if sess != nil && sess.PublicPort != 0 {
			assignedPort = sess.PublicPort
			break
		}
	}

	if assignedPort == 0 {
		t.Fatal("Agent failed to register and get public port in time")
	}

	t.Logf("Agent registered with public port %d", assignedPort)

	// 4. Test TCP forwarding through Relay
	tcpClient, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", assignedPort), 3*time.Second)
	if err != nil {
		t.Fatalf("Failed to dial Relay TCP public port: %v", err)
	}
	defer tcpClient.Close()

	testMsg := []byte("hello rdp over quic stream")
	if _, err := tcpClient.Write(testMsg); err != nil {
		t.Fatalf("Write to TCP client failed: %v", err)
	}

	replyBuf := make([]byte, len(testMsg))
	_ = tcpClient.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(tcpClient, replyBuf); err != nil {
		t.Fatalf("Read from TCP client failed: %v", err)
	}

	if string(replyBuf) != string(testMsg) {
		t.Fatalf("TCP echo mismatch: got %s, want %s", string(replyBuf), string(testMsg))
	}
	t.Log("TCP End-to-End forwarding test PASSED")

	// 5. Test UDP Datagram forwarding through Relay
	relayUDPAddr, _ := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", assignedPort))
	udpClient, err := net.DialUDP("udp", nil, relayUDPAddr)
	if err != nil {
		t.Fatalf("Failed to dial Relay UDP public port: %v", err)
	}
	defer udpClient.Close()

	udpTestMsg := []byte("hello rdp over quic datagram")
	if _, err := udpClient.Write(udpTestMsg); err != nil {
		t.Fatalf("Write to UDP client failed: %v", err)
	}

	udpReplyBuf := make([]byte, 2048)
	_ = udpClient.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := udpClient.Read(udpReplyBuf)
	if err != nil {
		t.Fatalf("Read from UDP client failed: %v", err)
	}

	if string(udpReplyBuf[:n]) != string(udpTestMsg) {
		t.Fatalf("UDP echo mismatch: got %s, want %s", string(udpReplyBuf[:n]), string(udpTestMsg))
	}
	t.Log("UDP Datagram End-to-End forwarding test PASSED")

	t.Log("ALL End-to-End Relay tests PASSED successfully!")
}
