package path

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"rdpulse/internal/protocol"
	"rdpulse/internal/punch"
	"rdpulse/internal/transport"
)

type datagramTransport struct {
	sent     chan []byte
	received chan []byte
}

func newDatagramTransport() *datagramTransport {
	return &datagramTransport{
		sent:     make(chan []byte, 8),
		received: make(chan []byte, 8),
	}
}

func (t *datagramTransport) OpenStream(context.Context) (transport.Stream, error) {
	return nil, errors.New("not implemented")
}

func (t *datagramTransport) AcceptStream(context.Context) (transport.Stream, error) {
	return nil, errors.New("not implemented")
}

func (t *datagramTransport) SendDatagram(payload []byte) error {
	t.sent <- append([]byte(nil), payload...)
	return nil
}

func (t *datagramTransport) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case payload := <-t.received:
		return payload, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *datagramTransport) RemoteAddr() net.Addr            { return &net.UDPAddr{} }
func (t *datagramTransport) LocalAddr() net.Addr             { return &net.UDPAddr{} }
func (t *datagramTransport) Stats() transport.TransportStats { return transport.TransportStats{} }
func (t *datagramTransport) Close() error                    { return nil }

func TestQUICRelayUDPFramesExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sessionID = uint32(0x80000042)
	tr := newDatagramTransport()
	mgr := NewManager(ctx, sessionID, []byte("session-token-at-least-32-bytes"), tr)
	defer mgr.Close()
	mgr.SetQUICRelay()

	payload := []byte("rdp udp payload")
	if err := mgr.SendUDP(payload); err != nil {
		t.Fatalf("send UDP over QUIC Relay: %v", err)
	}

	select {
	case datagram := <-tr.sent:
		hdr, encodedPayload, err := protocol.DecodeUDP(datagram)
		if err != nil {
			t.Fatalf("relay datagram is not a single UDP frame: %v", err)
		}
		if hdr.SessionID != sessionID || string(encodedPayload) != string(payload) {
			t.Fatalf("unexpected relay datagram: header=%+v payload=%q", hdr, encodedPayload)
		}
		tr.received <- datagram
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for QUIC Relay datagram")
	}

	received, err := mgr.ReceiveUDP(ctx)
	if err != nil {
		t.Fatalf("receive UDP over QUIC Relay: %v", err)
	}
	if string(received) != string(payload) {
		t.Fatalf("received payload %q, want %q", received, payload)
	}
}

func TestAddCandidatesDoesNotPromoteTCPPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := NewManager(ctx, 1, []byte("session-token-at-least-32-bytes"), newDatagramTransport())
	defer mgr.Close()

	mgr.AddCandidates([]protocol.CandidateInfo{{
		Protocol: "tcp",
		Type:     "lan",
		Address:  "192.0.2.10:3389",
		Priority: 1000,
	}})
	if got := mgr.TCPPath(); got != PathUnknown {
		t.Fatalf("AddCandidates promoted TCP path to %s, want Unknown", got)
	}
}

func TestFallbackUDPFromDirectReturnsToRelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := newDatagramTransport()
	mgr := NewManager(ctx, 1, []byte("session-token-at-least-32-bytes"), tr)
	defer mgr.Close()

	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	res := &punch.PunchResult{
		Conn:       conn,
		RemoteAddr: conn.LocalAddr().(*net.UDPAddr).AddrPort(),
	}
	mgr.SetDirectUDP(res, true)
	if got := mgr.UDPPath(); got != PathDirectLAN {
		t.Fatalf("UDP path %s, want LAN Direct", got)
	}

	mgr.FallbackUDPFromDirect()
	if got := mgr.UDPPath(); got != PathRelay {
		t.Fatalf("after fallback UDP path %s, want Relay", got)
	}
}

func TestParallelRaceActivatesRelayWhenP2PNotReady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := NewManager(ctx, 1, []byte("session-token-at-least-32-bytes"), newDatagramTransport())
	defer mgr.Close()

	ParallelRace(ctx, nil, []protocol.CandidateInfo{{
		Protocol: "tcp",
		Type:     "reflexive",
		Address:  "192.0.2.1:9",
		Priority: 100,
	}}, 1, []byte("session-token-at-least-32-bytes"), nil, mgr)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if mgr.TCPPath() == PathRelay && mgr.UDPPath() == PathRelay {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("after 300ms Happy-Eyeballs TCP=%s UDP=%s, want Relay", mgr.TCPPath(), mgr.UDPPath())
}
