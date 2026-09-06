package signaling

import (
	"net"
	"testing"

	"rdpulse/internal/protocol"
)

func TestCandidateExchangeUsesSessionScopedControllerRoute(t *testing.T) {
	controllerClient, controllerServer := net.Pipe()
	targetClient, targetServer := net.Pipe()
	defer controllerClient.Close()
	defer controllerServer.Close()
	defer targetClient.Close()
	defer targetServer.Close()

	coordinator := NewSessionCoordinator()
	coordinator.SetAuthorizer(func(controllerID, targetID string) bool {
		return controllerID == "controller" && targetID == "target"
	})
	targetWriter := protocol.NewControlWriter(targetServer)
	coordinator.RegisterPeer("target", "", nil, targetWriter, nil, 3389)
	controllerWriter := protocol.NewControlWriter(controllerServer)
	targetMessage := make(chan error, 1)
	go func() {
		_, readErr := protocol.ReadControlMessage(targetClient)
		targetMessage <- readErr
	}()
	response, err := coordinator.HandleConnectRequest(&ActivePeer{DeviceID: "controller", ControlWriter: controllerWriter}, &protocol.ControlMessage{
		TargetDeviceID: "target",
	}, "relay.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := <-targetMessage; err != nil {
		t.Fatalf("read connect notification: %v", err)
	}

	candidate := protocol.CandidateInfo{Protocol: "udp", Type: "lan", Address: "192.0.2.10:40000", Priority: 1}
	controllerMessage := make(chan *protocol.ControlMessage, 1)
	controllerError := make(chan error, 1)
	go func() {
		message, readErr := protocol.ReadControlMessage(controllerClient)
		controllerMessage <- message
		controllerError <- readErr
	}()
	if err := coordinator.ForwardCandidateExchange(response.SessionID, "target", "controller", []protocol.CandidateInfo{candidate}); err != nil {
		t.Fatal(err)
	}
	message, err := <-controllerMessage, <-controllerError
	if err != nil {
		t.Fatal(err)
	}
	if message.SessionID != response.SessionID || message.DeviceID != "target" || len(message.Candidates) != 1 {
		t.Fatalf("wrong session candidate message: %+v", message)
	}
}
