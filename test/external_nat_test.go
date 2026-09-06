package test

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"rdpulse/internal/agent"
	"rdpulse/internal/config"
	"rdpulse/internal/path"
)

const externalNATTimeout = 30 * time.Second

// TestExternalNATEndToEnd connects this test process to an already running
// Relay and controlled Agent. It is intentionally opt-in because a genuine NAT
// traversal test requires two independently routed networks.
func TestExternalNATEndToEnd(t *testing.T) {
	relayAddr := requireExternalEnv(t, "RDPULSE_EXTERNAL_RELAY_ADDR")
	rendezvousAddr := requireExternalEnv(t, "RDPULSE_EXTERNAL_RENDEZVOUS_ADDR")
	controllerID := requireExternalEnv(t, "RDPULSE_EXTERNAL_CONTROLLER_ID")
	controllerSecret := requireExternalEnv(t, "RDPULSE_EXTERNAL_CONTROLLER_SECRET")
	targetID := requireExternalEnv(t, "RDPULSE_EXTERNAL_TARGET_ID")

	cfg := &config.AgentConfig{}
	cfg.Server.Address = relayAddr
	cfg.Server.TLSAddress = externalEnv("RDPULSE_EXTERNAL_TLS_ADDR", relayAddr)
	cfg.Server.RendezvousAddress = rendezvousAddr
	cfg.Server.CACert = strings.TrimSpace(os.Getenv("RDPULSE_EXTERNAL_CA_CERT"))
	cfg.Server.InsecureSkipVerify = externalBoolEnv("RDPULSE_EXTERNAL_INSECURE_SKIP_VERIFY")
	cfg.Server.DisableQUIC = externalBoolEnv("RDPULSE_EXTERNAL_DISABLE_QUIC")
	cfg.Server.QUICDialTimeout = 4 * time.Second
	cfg.Device.ID = controllerID
	cfg.Device.Secret = controllerSecret
	cfg.Device.EnrollmentToken = strings.TrimSpace(os.Getenv("RDPULSE_EXTERNAL_ENROLLMENT_TOKEN"))
	cfg.RDP.Address = "127.0.0.1:3389"
	cfg.Transport.DatagramPayload = 1150
	cfg.Transport.DisableP2P = externalBoolEnv("RDPULSE_EXTERNAL_DISABLE_P2P")
	cfg.UDP.SessionIdleTimeout = 60 * time.Second
	cfg.UDP.ReassemblyTimeout = 100 * time.Millisecond
	cfg.UDP.MaxFragments = 64
	cfg.Heartbeat.Interval = 10 * time.Second
	cfg.Reconnect.MaxInterval = 30 * time.Second

	if len(controllerSecret) < 32 {
		t.Fatal("RDPULSE_EXTERNAL_CONTROLLER_SECRET must contain at least 32 bytes")
	}
	if token := cfg.Device.EnrollmentToken; token != "" && len(token) < 32 {
		t.Fatal("RDPULSE_EXTERNAL_ENROLLMENT_TOKEN must contain at least 32 bytes")
	}
	if cfg.Server.CACert == "" && !cfg.Server.InsecureSkipVerify {
		t.Fatal("set RDPULSE_EXTERNAL_CA_CERT or explicitly set RDPULSE_EXTERNAL_INSECURE_SKIP_VERIFY=true")
	}

	tcpReservation, udpReservation, localAddr, err := listenTCPAndUDP()
	if err != nil {
		t.Fatalf("reserve local proxy address: %v", err)
	}
	_ = tcpReservation.Close()
	_ = udpReservation.Close()

	ctx, cancel := context.WithTimeout(context.Background(), externalNATTimeout)
	defer cancel()
	proxy, pathManager, err := agent.NewClient(cfg).ConnectTarget(ctx, targetID, localAddr, false)
	if err != nil {
		t.Fatalf("connect external target %q: %v", targetID, err)
	}
	defer proxy.Close()
	defer pathManager.Close()

	expectedTCP := strings.ToLower(externalEnv("RDPULSE_EXTERNAL_EXPECT_TCP", "any"))
	expectedUDP := strings.ToLower(externalEnv("RDPULSE_EXTERNAL_EXPECT_UDP", "any"))
	if err := waitForExpectedExternalPaths(ctx, pathManager, expectedTCP, expectedUDP); err != nil {
		t.Fatal(err)
	}

	if expectedTCP == "direct" {
		directCtx, directCancel := context.WithTimeout(ctx, 5*time.Second)
		directConn, err := pathManager.DialDirectTCP(directCtx)
		directCancel()
		if err != nil {
			t.Fatalf("authenticated direct TCP dial failed: %v", err)
		}
		if err := probeExternalRDP(directConn); err != nil {
			_ = directConn.Close()
			t.Fatalf("direct TCP RDP negotiation failed: %v", err)
		}
		_ = directConn.Close()
	}

	// An X.224/RDP negotiation through the local proxy verifies that the selected
	// direct or Relay stream reaches the target RDP service end to end.
	conn, err := net.DialTimeout("tcp", localAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial local controller proxy: %v", err)
	}
	if err := probeExternalRDP(conn); err != nil {
		_ = conn.Close()
		t.Fatalf("selected TCP path RDP negotiation failed: %v", err)
	}
	_ = conn.Close()
	t.Logf("external NAT test passed: TCP=%s UDP=%s", pathManager.TCPPath(), pathManager.UDPPath())
}

func probeExternalRDP(conn net.Conn) error {
	// TPKT + X.224 Connection Request + RDP Negotiation Request. Requested
	// protocols are TLS and CredSSP; no credentials or desktop session is used.
	request := []byte{
		0x03, 0x00, 0x00, 0x13,
		0x0e, 0xe0, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x08, 0x00, 0x03, 0x00, 0x00, 0x00,
	}
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	if _, err := conn.Write(request); err != nil {
		return err
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return err
	}
	responseLength := int(binary.BigEndian.Uint16(header[2:4]))
	if header[0] != 3 || responseLength < len(header) || responseLength > 4096 {
		return fmt.Errorf("invalid TPKT response header %x", header)
	}
	response := make([]byte, responseLength-len(header))
	if _, err := io.ReadFull(conn, response); err != nil {
		return err
	}
	return nil
}

func requireExternalEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Skipf("external NAT environment unavailable: %s is not set", name)
	}
	return value
}

func externalEnv(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func externalBoolEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func waitForExpectedExternalPaths(ctx context.Context, manager *path.Manager, expectedTCP, expectedUDP string) error {
	if !validExternalExpectation(expectedTCP, false) {
		return fmt.Errorf("invalid RDPULSE_EXTERNAL_EXPECT_TCP=%q; use any, direct, or relay", expectedTCP)
	}
	if !validExternalExpectation(expectedUDP, true) {
		return fmt.Errorf("invalid RDPULSE_EXTERNAL_EXPECT_UDP=%q; use any, direct, relay, or disabled", expectedUDP)
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		tcpPath, udpPath := manager.TCPPath(), manager.UDPPath()
		if externalPathMatches(tcpPath, expectedTCP, false) && externalPathMatches(udpPath, expectedUDP, true) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for paths TCP=%q UDP=%q; last paths TCP=%s UDP=%s", expectedTCP, expectedUDP, tcpPath, udpPath)
		case <-ticker.C:
		}
	}
}

func validExternalExpectation(expected string, allowDisabled bool) bool {
	return expected == "any" || expected == "direct" || expected == "relay" || allowDisabled && expected == "disabled"
}

func externalPathMatches(actual path.PathType, expected string, allowDisabled bool) bool {
	switch expected {
	case "any":
		return actual != path.PathUnknown
	case "direct":
		if allowDisabled {
			return actual == path.PathDirectLAN || actual == path.PathDirectUDP
		}
		return actual == path.PathDirectLAN || actual == path.PathDirectTCP
	case "relay":
		return actual == path.PathRelay
	case "disabled":
		return allowDisabled && actual == path.PathDisabled
	default:
		return false
	}
}
