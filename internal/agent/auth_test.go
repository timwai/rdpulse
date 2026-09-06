package agent

import (
	"net"
	"testing"

	"rdpulse/internal/config"
	"rdpulse/internal/protocol"
	"rdpulse/internal/transport"
)

type authTestStream struct{ net.Conn }

func (*authTestStream) CancelRead(uint64)  {}
func (*authTestStream) CancelWrite(uint64) {}

var _ transport.Stream = (*authTestStream)(nil)

func TestAuthenticateControlSendsInvitationOnlyAfterChallenge(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	cfg := &config.AgentConfig{}
	cfg.Device.ID = "device-a"
	cfg.Device.Secret = "device-secret-at-least-32-bytes-long"
	cfg.Device.EnrollmentToken = "device-invitation-at-least-32-bytes-long"
	client := NewClient(cfg)

	serverDone := make(chan error, 1)
	go func() {
		first, err := protocol.ReadControlMessage(serverConn)
		if err != nil {
			serverDone <- err
			return
		}
		if first.EnrollmentToken != "" {
			t.Errorf("normal auth exposed enrollment invitation")
		}
		writer := protocol.NewControlWriter(serverConn)
		if err := writer.WriteMessage(&protocol.ControlMessage{Type: protocol.MsgTypeError, Code: 428}); err != nil {
			serverDone <- err
			return
		}
		second, err := protocol.ReadControlMessage(serverConn)
		if err != nil {
			serverDone <- err
			return
		}
		if second.EnrollmentToken != cfg.Device.EnrollmentToken {
			t.Errorf("challenge response did not contain configured invitation")
		}
		serverDone <- writer.WriteMessage(&protocol.ControlMessage{Type: protocol.MsgTypeAuthOK})
	}()

	stream := &authTestStream{Conn: clientConn}
	if err := client.authenticateControl(stream, protocol.NewControlWriter(stream)); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
