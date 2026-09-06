package test

import (
	"context"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"rdpulse/internal/acl"
	"rdpulse/internal/agent"
	"rdpulse/internal/config"
	"rdpulse/internal/path"
	"rdpulse/internal/relay"
	"rdpulse/internal/rendezvous"
	"rdpulse/internal/storage"
	"rdpulse/internal/transport/quicgo"
	"rdpulse/internal/transport/tlsmux"
)

func TestP2PAndControllerEndToEnd(t *testing.T) {
	runControllerEndToEnd(t, false, false)
}

func TestForcedQUICRelayControllerEndToEnd(t *testing.T) {
	runControllerEndToEnd(t, true, false)
}

func TestForcedTLSRelayControllerEndToEnd(t *testing.T) {
	runControllerEndToEnd(t, true, true)
}

func runControllerEndToEnd(t *testing.T, disableP2P, forceTLS bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Start mock RDP server on localhost
	mockTCPListener, mockUDPConn, mockRDPAddr, err := listenTCPAndUDP()
	if err != nil {
		t.Fatalf("Listen mock TCP/UDP failed: %v", err)
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
				_, _ = io.Copy(c, c) // Echo
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
			_, _ = mockUDPConn.WriteTo(buf[:n], remoteAddr) // Echo
		}
	}()

	// 2. Start Relay Server with Rendezvous
	dbPath := "test_p2p_relay.db"
	defer os.Remove(dbPath)
	defer os.Remove(dbPath + "-wal")
	defer os.Remove(dbPath + "-shm")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("Open storage failed: %v", err)
	}
	defer db.Close()

	portMgr, err := relay.NewPortManager(32000, 32100, db)
	if err != nil {
		t.Fatalf("Create port manager failed: %v", err)
	}

	aclMgr, err := acl.NewManager("allow", nil)
	if err != nil {
		t.Fatalf("Create ACL manager failed: %v", err)
	}

	relayCfg := &config.RelayConfig{}
	relayCfg.UDP.DatagramPayload = 1150
	relayCfg.UDP.SessionIdleTimeout = 5 * time.Second
	relayCfg.UDP.ReassemblyTimeout = 100 * time.Millisecond
	relayCfg.UDP.MaxFragments = 64
	relayCfg.RDP.PublicHost = "127.0.0.1"
	targetEnrollmentToken := "target-enrollment-token-at-least-32-bytes"
	controllerEnrollmentToken := "controller-enrollment-token-at-least-32-bytes"
	relayCfg.Security.EnrollmentInvitations = []config.EnrollmentInvitation{
		{DeviceID: "PC-TARGET-B", Token: targetEnrollmentToken, ExpiresAt: time.Now().Add(time.Hour)},
		{DeviceID: "PC-CONTROLLER-A", Token: controllerEnrollmentToken, ExpiresAt: time.Now().Add(time.Hour)},
		{DeviceID: "PC-CONTROLLER-C", Token: "controller-c-enrollment-token-at-least-32-bytes", ExpiresAt: time.Now().Add(time.Hour)},
	}
	relayCfg.Security.ControllerAccess = map[string][]string{
		"PC-TARGET-B": {"PC-CONTROLLER-A", "PC-CONTROLLER-C"},
	}

	agentMgr := relay.NewAgentManager(relayCfg, db, portMgr, aclMgr)
	defer agentMgr.Close()

	tlsConf, err := quicgo.GenerateSelfSignedCert()
	if err != nil {
		t.Fatalf("Generate cert failed: %v", err)
	}

	quicListener, err := quicgo.ListenAddr("127.0.0.1:0", tlsConf, quicgo.DefaultQUICConfig())
	if err != nil {
		t.Fatalf("Listen QUIC failed: %v", err)
	}
	defer quicListener.Close()
	relayAddr := quicListener.Addr().String()
	tlsListener, err := tlsmux.Listen("127.0.0.1:0", tlsConf)
	if err != nil {
		t.Fatalf("Listen TLS fallback failed: %v", err)
	}
	defer tlsListener.Close()

	rdzvServer, err := rendezvous.StartServer(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start rendezvous failed: %v", err)
	}
	defer rdzvServer.Close()

	go func() {
		for {
			tr, err := quicListener.Accept(ctx)
			if err != nil {
				return
			}
			go agentMgr.HandleIncomingConnection(ctx, tr)
		}
	}()
	go func() {
		for {
			tr, err := tlsListener.Accept(ctx)
			if err != nil {
				return
			}
			go agentMgr.HandleIncomingConnection(ctx, tr)
		}
	}()

	// 3. Start Controlled Agent (PC-B)
	controlledCfg := &config.AgentConfig{}
	controlledCfg.Server.Address = relayAddr
	controlledCfg.Server.TLSAddress = tlsListener.Addr().String()
	controlledCfg.Server.DisableQUIC = forceTLS
	controlledCfg.Server.RendezvousAddress = rdzvServer.Addr().String()
	controlledCfg.Server.InsecureSkipVerify = true
	controlledCfg.Device.ID = "PC-TARGET-B"
	controlledCfg.Device.Secret = "target-b-secret-at-least-32-bytes"
	controlledCfg.Device.EnrollmentToken = targetEnrollmentToken
	controlledCfg.RDP.Address = mockRDPAddr
	controlledCfg.Transport.DatagramPayload = 1150
	controlledCfg.Transport.DisableP2P = disableP2P
	controlledCfg.UDP.SessionIdleTimeout = 5 * time.Second
	controlledCfg.UDP.ReassemblyTimeout = 100 * time.Millisecond
	controlledCfg.UDP.MaxFragments = 64
	controlledCfg.Heartbeat.Interval = 2 * time.Second
	controlledCfg.Reconnect.MaxInterval = 5 * time.Second

	controlledAgent := agent.NewClient(controlledCfg)
	go func() {
		_ = controlledAgent.Run(ctx)
	}()

	// Wait for Controlled Agent to register on Relay
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if sess := agentMgr.GetAgentByDeviceID("PC-TARGET-B"); sess != nil {
			break
		}
	}

	if agentMgr.GetAgentByDeviceID("PC-TARGET-B") == nil {
		t.Fatal("Controlled Agent PC-TARGET-B failed to register in time")
	}
	t.Log("Controlled Agent PC-TARGET-B registered successfully on Relay")

	// 4. Start Controller Agent (PC-A) connecting to PC-B
	controllerCfg := &config.AgentConfig{}
	controllerCfg.Server.Address = relayAddr
	controllerCfg.Server.TLSAddress = tlsListener.Addr().String()
	controllerCfg.Server.DisableQUIC = forceTLS
	controllerCfg.Server.RendezvousAddress = rdzvServer.Addr().String()
	controllerCfg.Server.InsecureSkipVerify = true
	controllerCfg.Device.ID = "PC-CONTROLLER-A"
	controllerCfg.Device.Secret = "controller-a-secret-at-least-32-bytes"
	controllerCfg.Device.EnrollmentToken = controllerEnrollmentToken
	controllerCfg.RDP.Address = "127.0.0.1:3389"
	controllerCfg.Transport.DatagramPayload = 1150
	controllerCfg.Transport.DisableP2P = disableP2P
	controllerCfg.UDP.SessionIdleTimeout = 5 * time.Second
	controllerCfg.UDP.ReassemblyTimeout = 100 * time.Millisecond
	controllerCfg.UDP.MaxFragments = 64
	controllerCfg.Heartbeat.Interval = 2 * time.Second
	controllerCfg.Reconnect.MaxInterval = 5 * time.Second

	controllerClient := agent.NewClient(controllerCfg)

	// Pick a free local port for controller proxy
	proxyListen, proxyUDPReservation, localProxyAddr, err := listenTCPAndUDP()
	if err != nil {
		t.Fatalf("Listen local proxy port failed: %v", err)
	}
	_ = proxyListen.Close()
	_ = proxyUDPReservation.Close()

	proxy, pathMgr, err := controllerClient.ConnectTarget(ctx, "PC-TARGET-B", localProxyAddr, false)
	if err != nil {
		t.Fatalf("ConnectTarget failed: %v", err)
	}
	defer proxy.Close()
	defer pathMgr.Close()

	t.Logf("Controller local proxy established on %s", localProxyAddr)

	// 5. Allow Happy-Eyeballs to settle
	time.Sleep(600 * time.Millisecond)
	t.Logf("Path Status: TCP=%s, UDP=%s", pathMgr.TCPPath(), pathMgr.UDPPath())
	if disableP2P {
		expectedUDP := path.PathRelay
		if forceTLS {
			expectedUDP = path.PathDisabled
		}
		if pathMgr.TCPPath() != path.PathRelay || pathMgr.UDPPath() != expectedUDP {
			t.Fatalf("expected forced fallback paths, got TCP=%s UDP=%s", pathMgr.TCPPath(), pathMgr.UDPPath())
		}
	} else if pathMgr.TCPPath() != path.PathDirectLAN && pathMgr.TCPPath() != path.PathDirectTCP {
		t.Fatalf("expected TCP to use an authenticated direct path, got %s", pathMgr.TCPPath())
	}

	// 6. Test TCP connection through Controller Local Proxy -> Target RDP
	tcpClient, err := net.DialTimeout("tcp", localProxyAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("Dial local proxy TCP failed: %v", err)
	}
	defer tcpClient.Close()

	testMsg := []byte("p2p test tcp message over controller proxy")
	if _, err := tcpClient.Write(testMsg); err != nil {
		t.Fatalf("Write TCP client failed: %v", err)
	}

	replyBuf := make([]byte, len(testMsg))
	_ = tcpClient.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(tcpClient, replyBuf); err != nil {
		t.Fatalf("Read TCP reply failed: %v", err)
	}

	if string(replyBuf) != string(testMsg) {
		t.Fatalf("TCP echo mismatch: got %s, want %s", string(replyBuf), string(testMsg))
	}
	t.Log("Controller TCP forwarding test PASSED")
	if !disableP2P {
		secondTCP, err := net.DialTimeout("tcp", localProxyAddr, 3*time.Second)
		if err != nil {
			t.Fatalf("Dial second direct TCP connection failed: %v", err)
		}
		defer secondTCP.Close()
		secondMessage := []byte("independent concurrent direct tcp connection")
		if _, err := secondTCP.Write(secondMessage); err != nil {
			t.Fatal(err)
		}
		secondReply := make([]byte, len(secondMessage))
		_ = secondTCP.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(secondTCP, secondReply); err != nil || string(secondReply) != string(secondMessage) {
			t.Fatalf("second direct TCP echo=%q err=%v", secondReply, err)
		}
		t.Log("Concurrent independent direct TCP connections PASSED")
	}

	// 7. TLS/TCP fallback intentionally disables RDP UDP. QUIC and P2P paths
	// continue to test UDP end-to-end.
	if forceTLS {
		t.Log("SUCCESS: TCP traffic confirmed over forced TLS/TCP Relay; UDP correctly disabled")
		return
	}

	// Test UDP packet through Controller Local Proxy -> Target RDP
	udpMsg := []byte("p2p test udp datagram over controller proxy")
	assertUDPEcho(t, localProxyAddr, udpMsg)
	t.Log("Controller UDP datagram forwarding test PASSED")

	if !disableP2P {
		// Keep the first session alive while a second Controller establishes its
		// own direct UDP path through the target's shared socket dispatcher.
		secondCfg := *controllerCfg
		secondCfg.Device.ID = "PC-CONTROLLER-C"
		secondCfg.Device.Secret = "controller-c-secret-at-least-32-bytes"
		secondCfg.Device.EnrollmentToken = "controller-c-enrollment-token-at-least-32-bytes"
		secondClient := agent.NewClient(&secondCfg)
		reservationTCP, reservationUDP, secondProxyAddr, err := listenTCPAndUDP()
		if err != nil {
			t.Fatal(err)
		}
		_ = reservationTCP.Close()
		_ = reservationUDP.Close()
		secondProxy, secondPath, err := secondClient.ConnectTarget(ctx, "PC-TARGET-B", secondProxyAddr, false)
		if err != nil {
			t.Fatalf("second Controller ConnectTarget failed: %v", err)
		}
		defer secondProxy.Close()
		defer secondPath.Close()
		time.Sleep(400 * time.Millisecond)
		assertUDPEcho(t, secondProxyAddr, []byte("second concurrent p2p session"))
		assertUDPEcho(t, localProxyAddr, []byte("first p2p session remains active"))
		if secondPath.UDPPath() != path.PathDirectLAN && secondPath.UDPPath() != path.PathDirectUDP {
			t.Fatalf("second Controller did not establish direct UDP: %s", secondPath.UDPPath())
		}
		t.Log("Concurrent direct UDP sessions PASSED")
	}

	// Check P2P status
	if disableP2P && pathMgr.UDPPath() == path.PathRelay {
		t.Log("SUCCESS: UDP traffic confirmed over forced QUIC Relay")
	} else if pathMgr.UDPPath() == path.PathDirectLAN || pathMgr.UDPPath() == path.PathDirectUDP {
		t.Log("SUCCESS: UDP traffic confirmed directly flowing over P2P Direct!")
	} else {
		t.Fatalf("expected authenticated direct UDP path, got %s", pathMgr.UDPPath())
	}
}

func assertUDPEcho(t *testing.T, proxyAddress string, payload []byte) {
	t.Helper()
	proxyUDPAddr, err := net.ResolveUDPAddr("udp", proxyAddress)
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.DialUDP("udp", nil, proxyUDPAddr)
	if err != nil {
		t.Fatalf("Dial local proxy UDP failed: %v", err)
	}
	defer client.Close()
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("Write UDP client failed: %v", err)
	}
	reply := make([]byte, 2048)
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := client.Read(reply)
	if err != nil {
		t.Fatalf("Read UDP reply failed: %v", err)
	}
	if string(reply[:n]) != string(payload) {
		t.Fatalf("UDP echo mismatch: got %s, want %s", string(reply[:n]), string(payload))
	}
}
