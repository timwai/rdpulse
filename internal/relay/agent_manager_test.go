package relay

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"rdpulse/internal/config"
	"rdpulse/internal/protocol"
	"rdpulse/internal/storage"
	"rdpulse/internal/transport"
)

const (
	testEnrollmentToken = "test-enrollment-token-at-least-32-bytes"
	testDeviceSecret    = "test-device-secret-at-least-32-bytes"
)

type testStream struct {
	net.Conn
}

func (s *testStream) CancelRead(uint64)  {}
func (s *testStream) CancelWrite(uint64) {}

type testTransport struct {
	stream transport.Stream
}

func (t *testTransport) OpenStream(context.Context) (transport.Stream, error) {
	return nil, errors.New("not implemented")
}

func (t *testTransport) AcceptStream(context.Context) (transport.Stream, error) {
	return t.stream, nil
}

func (t *testTransport) SendDatagram([]byte) error {
	return errors.New("not implemented")
}

func (t *testTransport) ReceiveDatagram(context.Context) ([]byte, error) {
	return nil, errors.New("not implemented")
}

func (t *testTransport) RemoteAddr() net.Addr            { return t.stream.(*testStream).RemoteAddr() }
func (t *testTransport) LocalAddr() net.Addr             { return t.stream.(*testStream).LocalAddr() }
func (t *testTransport) Stats() transport.TransportStats { return transport.TransportStats{} }
func (t *testTransport) Close() error                    { return t.stream.Close() }

func newAuthenticationTestManager(t *testing.T) (*AgentManager, *storage.DB) {
	t.Helper()
	db, err := storage.Open(t.TempDir() + "/relay.db")
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := &config.RelayConfig{}
	cfg.Security.EnrollmentInvitations = []config.EnrollmentInvitation{
		{DeviceID: "unknown-device", Token: testEnrollmentToken, ExpiresAt: time.Now().Add(time.Hour)},
		{DeviceID: "controller-a", Token: testEnrollmentToken, ExpiresAt: time.Now().Add(time.Hour)},
	}
	return NewAgentManager(cfg, db, nil, nil), db
}

func runAuthenticationExchange(t *testing.T, manager *AgentManager, messages ...*protocol.ControlMessage) []*protocol.ControlMessage {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })

	done := make(chan struct{})
	go func() {
		manager.HandleIncomingConnection(context.Background(), &testTransport{stream: &testStream{Conn: serverConn}})
		close(done)
	}()

	writer := protocol.NewControlWriter(clientConn)
	responses := make([]*protocol.ControlMessage, 0, len(messages))
	for _, msg := range messages {
		if err := writer.WriteMessage(msg); err != nil {
			t.Fatalf("write control message: %v", err)
		}
		resp, err := protocol.ReadControlMessage(clientConn)
		if err != nil {
			t.Fatalf("read control response: %v", err)
		}
		responses = append(responses, resp)
	}

	_ = clientConn.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("relay authentication handler did not exit")
	}
	return responses
}

func TestUnknownDeviceRequiresEnrollmentToken(t *testing.T) {
	manager, _ := newAuthenticationTestManager(t)
	responses := runAuthenticationExchange(t, manager, &protocol.ControlMessage{
		Type:     protocol.MsgTypeAuth,
		DeviceID: "unknown-device",
		Secret:   testDeviceSecret,
	})

	if got := responses[0]; got.Type != protocol.MsgTypeError || got.Code != 428 {
		t.Fatalf("got response %+v, want enrollment rejection", got)
	}
}

func TestUnknownDeviceEnrollsWithSecretAsToken(t *testing.T) {
	manager, db := newAuthenticationTestManager(t)
	manager.UpdateAccessPolicy(true, nil)
	if err := db.UpsertEnrollmentInvitation("simple-device", testDeviceSecret, time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	responses := runAuthenticationExchange(t, manager,
		&protocol.ControlMessage{
			Type:     protocol.MsgTypeAuth,
			DeviceID: "simple-device",
			Secret:   testDeviceSecret,
		},
		&protocol.ControlMessage{
			Type:           protocol.MsgTypeConnectRequest,
			DeviceID:       "simple-device",
			TargetDeviceID: "target-b",
		},
	)

	if got := responses[0]; got.Type != protocol.MsgTypeAuthOK {
		t.Fatalf("got response %+v, want MsgTypeAuthOK", got)
	}

	record, err := db.GetAgentByDeviceID("simple-device")
	if err != nil || record == nil {
		t.Fatalf("agent record not found: %v", err)
	}
	if record.SecretHash != hashSecret(testDeviceSecret) {
		t.Fatalf("unexpected secret hash: %s", record.SecretHash)
	}
}

func TestDisabledDeviceIsRejected(t *testing.T) {
	manager, db := newAuthenticationTestManager(t)
	if err := db.UpsertAgent(&storage.AgentRecord{
		DeviceID:   "disabled-device",
		SecretHash: hashSecret(testDeviceSecret),
		Enabled:    false,
	}); err != nil {
		t.Fatalf("insert disabled agent: %v", err)
	}

	responses := runAuthenticationExchange(t, manager, &protocol.ControlMessage{
		Type:     protocol.MsgTypeAuth,
		DeviceID: "disabled-device",
		Secret:   testDeviceSecret,
	})

	if got := responses[0]; got.Type != protocol.MsgTypeError || got.Code != 403 {
		t.Fatalf("got response %+v, want disabled-device rejection", got)
	}
}

func TestUnauthorizedControllerTargetIsRejected(t *testing.T) {
	manager, _ := newAuthenticationTestManager(t)
	responses := runAuthenticationExchange(t, manager,
		&protocol.ControlMessage{
			Type:            protocol.MsgTypeAuth,
			DeviceID:        "controller-a",
			Secret:          testDeviceSecret,
			EnrollmentToken: testEnrollmentToken,
		},
		&protocol.ControlMessage{
			Type:           protocol.MsgTypeConnectRequest,
			DeviceID:       "controller-a",
			TargetDeviceID: "target-b",
		},
	)

	if got := responses[0]; got.Type != protocol.MsgTypeAuthOK {
		t.Fatalf("got auth response %+v, want AUTH_OK", got)
	}
	if got := responses[1]; got.Type != protocol.MsgTypeError || got.Code != 403 {
		t.Fatalf("got response %+v, want controller authorization rejection", got)
	}
}

func TestRegistrationUpdatePreservesAdministrativeState(t *testing.T) {
	_, db := newAuthenticationTestManager(t)
	const originalHash = "original-secret-hash"
	if err := db.UpsertAgent(&storage.AgentRecord{
		DeviceID:   "managed-device",
		Hostname:   "old-host",
		SecretHash: originalHash,
		PublicPort: 20001,
		Enabled:    false,
	}); err != nil {
		t.Fatalf("insert managed device: %v", err)
	}
	if err := db.UpdateAgentRegistration("managed-device", "new-host", 20002); err != nil {
		t.Fatalf("update registration: %v", err)
	}

	record, err := db.GetAgentByDeviceID("managed-device")
	if err != nil {
		t.Fatalf("read managed device: %v", err)
	}
	if record.SecretHash != originalHash || record.Enabled {
		t.Fatalf("registration update changed credential or enabled state: %+v", record)
	}
	if record.Hostname != "new-host" || record.PublicPort != 20002 {
		t.Fatalf("registration fields were not updated: %+v", record)
	}
}

func TestCreateAgentCannotReplaceCredential(t *testing.T) {
	_, db := newAuthenticationTestManager(t)
	first := &storage.AgentRecord{
		DeviceID:   "claimed-device",
		SecretHash: "first-secret-hash",
		Enabled:    true,
	}
	if err := db.CreateAgent(first); err != nil {
		t.Fatalf("create first agent: %v", err)
	}
	if err := db.CreateAgent(&storage.AgentRecord{
		DeviceID:   first.DeviceID,
		SecretHash: "replacement-secret-hash",
		Enabled:    true,
	}); err == nil {
		t.Fatal("expected duplicate enrollment to be rejected")
	}

	record, err := db.GetAgentByDeviceID(first.DeviceID)
	if err != nil {
		t.Fatalf("read enrolled agent: %v", err)
	}
	if record.SecretHash != first.SecretHash {
		t.Fatalf("duplicate enrollment replaced credential: %+v", record)
	}
}

type datagramRouteTransport struct {
	received chan []byte
	sent     chan []byte
}

func newDatagramRouteTransport() *datagramRouteTransport {
	return &datagramRouteTransport{
		received: make(chan []byte, 2),
		sent:     make(chan []byte, 2),
	}
}

func (t *datagramRouteTransport) OpenStream(context.Context) (transport.Stream, error) {
	return nil, errors.New("not implemented")
}

func (t *datagramRouteTransport) AcceptStream(context.Context) (transport.Stream, error) {
	return nil, errors.New("not implemented")
}

func (t *datagramRouteTransport) SendDatagram(payload []byte) error {
	t.sent <- append([]byte(nil), payload...)
	return nil
}

func (t *datagramRouteTransport) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case payload := <-t.received:
		return payload, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *datagramRouteTransport) RemoteAddr() net.Addr            { return &net.UDPAddr{} }
func (t *datagramRouteTransport) LocalAddr() net.Addr             { return &net.UDPAddr{} }
func (t *datagramRouteTransport) Stats() transport.TransportStats { return transport.TransportStats{} }
func (t *datagramRouteTransport) Close() error                    { return nil }

func TestControllerDatagramsRouteBothDirections(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sessionID = uint32(0x80000042)
	controllerTr := newDatagramRouteTransport()
	targetTr := newDatagramRouteTransport()
	target := &AgentSession{Transport: targetTr, ctx: ctx}
	target.registerControllerRoute(sessionID, controllerTr)
	target.StartUDPReceiveFromAgent()

	manager := &AgentManager{}
	go manager.runControllerDatagramLoop(ctx, controllerTr, target, sessionID)

	datagram := protocol.EncodeUDP(nil, sessionID, 1, 0, 1, []byte("payload"))
	controllerTr.received <- datagram
	select {
	case got := <-targetTr.sent:
		if !bytes.Equal(got, datagram) {
			t.Fatalf("controller-to-target datagram changed: %x", got)
		}
	case <-time.After(time.Second):
		t.Fatal("controller datagram was not routed to target")
	}

	targetTr.received <- datagram
	select {
	case got := <-controllerTr.sent:
		if !bytes.Equal(got, datagram) {
			t.Fatalf("target-to-controller datagram changed: %x", got)
		}
	case <-time.After(time.Second):
		t.Fatal("target datagram was not routed to controller")
	}
}
