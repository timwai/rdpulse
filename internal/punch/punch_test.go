package punch

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"rdpulse/internal/protocol"
)

func TestUDPPunchingSimulation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sessionID := uint64(987654321)
	sessionToken := []byte("secret-punch-token-123")

	// Peer 1 UDP Conn
	conn1, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP conn1 failed: %v", err)
	}
	defer conn1.Close()

	// Peer 2 UDP Conn
	conn2, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP conn2 failed: %v", err)
	}
	defer conn2.Close()

	cand1 := []protocol.CandidateInfo{{
		Protocol: "udp",
		Type:     "lan",
		Address:  conn1.LocalAddr().String(),
		Priority: 1000,
	}}

	cand2 := []protocol.CandidateInfo{{
		Protocol: "udp",
		Type:     "lan",
		Address:  conn2.LocalAddr().String(),
		Priority: 1000,
	}}

	var wg sync.WaitGroup
	var res1, res2 *PunchResult
	var err1, err2 error

	wg.Add(2)
	go func() {
		defer wg.Done()
		res1, err1 = PunchUDP(ctx, conn1, cand2, sessionID, sessionToken, 1*time.Second)
	}()

	go func() {
		defer wg.Done()
		res2, err2 = PunchUDP(ctx, conn2, cand1, sessionID, sessionToken, 1*time.Second)
	}()

	wg.Wait()

	if err1 != nil {
		t.Fatalf("Peer 1 punch failed: %v", err1)
	}
	if err2 != nil {
		t.Fatalf("Peer 2 punch failed: %v", err2)
	}
	defer res1.Close()
	defer res2.Close()

	t.Logf("Peer 1 connected to %s", res1.RemoteAddr)
	t.Logf("Peer 2 connected to %s", res2.RemoteAddr)
}

func TestTCPPunchingSimulation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sessionID := uint64(123456789)
	sessionToken := []byte("secret-tcp-token-456")

	// Start a simulated TCP listener for peer 2
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen tcp failed: %v", err)
	}
	defer l.Close()

	peer2Addr := l.Addr().String()
	cand2 := []protocol.CandidateInfo{{
		Protocol: "tcp",
		Type:     "lan",
		Address:  peer2Addr,
		Priority: 1000,
	}}

	// Peer 2 accepts and authenticates
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = AcceptTCPPeer(conn, func(got uint64) ([]byte, bool) {
			return sessionToken, got == sessionID
		})
	}()

	// Peer 1 dials and authenticates via PunchTCP
	conn, err := PunchTCP(ctx, cand2, sessionID, sessionToken, 1*time.Second)
	if err != nil {
		t.Fatalf("PunchTCP failed: %v", err)
	}
	defer conn.Close()

	t.Log("TCP punch authentication simulation passed")
}

func TestTCPAuthRejectsReplayedClientTag(t *testing.T) {
	sessionID := uint64(424242)
	token := []byte("tcp-replay-token-at-least-32-bytes")

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	acceptErr := make(chan error, 2)
	go func() {
		for i := 0; i < 2; i++ {
			conn, err := l.Accept()
			if err != nil {
				acceptErr <- err
				return
			}
			_, err = AcceptTCPPeer(conn, func(got uint64) ([]byte, bool) {
				return token, got == sessionID
			})
			_ = conn.Close()
			acceptErr <- err
		}
	}()

	ctx := context.Background()
	cand := []protocol.CandidateInfo{{
		Protocol: "tcp",
		Type:     "lan",
		Address:  l.Addr().String(),
		Priority: 1000,
	}}
	conn, err := PunchTCP(ctx, cand, sessionID, token, time.Second)
	if err != nil {
		t.Fatalf("legitimate PunchTCP failed: %v", err)
	}
	_ = conn.Close()
	if err := <-acceptErr; err != nil {
		t.Fatalf("legitimate accept failed: %v", err)
	}

	attack, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial replay: %v", err)
	}
	replay := tcpAuthTag(sessionID, token, 1, nil)
	if _, err := attack.Write(replay[:]); err != nil {
		t.Fatalf("write replay: %v", err)
	}
	_ = attack.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, _ = attack.Read(make([]byte, 64))
	_ = attack.Close()

	if err := <-acceptErr; err == nil {
		t.Fatal("replayed static TCP auth tag was accepted")
	}
}

func TestTCPProbeDoesNotOpenDataSession(t *testing.T) {
	sessionID := uint64(777)
	token := []byte("tcp-probe-token-at-least-32-bytes!")

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	done := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_, err = AcceptTCPPeer(conn, func(got uint64) ([]byte, bool) {
			return token, got == sessionID
		})
		done <- err
	}()

	err = ProbeTCP(context.Background(), []protocol.CandidateInfo{{
		Protocol: "tcp",
		Type:     "lan",
		Address:  l.Addr().String(),
		Priority: 1000,
	}}, sessionID, token, time.Second)
	if err != nil {
		t.Fatalf("ProbeTCP failed: %v", err)
	}
	if err := <-done; !errors.Is(err, ErrTCPProbe) {
		t.Fatalf("AcceptTCPPeer error = %v, want ErrTCPProbe", err)
	}
}

