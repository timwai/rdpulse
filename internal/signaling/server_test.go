package signaling

import (
	"bytes"
	"testing"

	"rdpulse/internal/protocol"
)

func TestOldSessionCannotUnregisterReplacementPeer(t *testing.T) {
	coordinator := NewSessionCoordinator()
	oldWriter := protocol.NewControlWriter(&bytes.Buffer{})
	newWriter := protocol.NewControlWriter(&bytes.Buffer{})

	coordinator.RegisterPeer("device-a", "old-host", nil, oldWriter, nil, 20001)
	newPeer := coordinator.RegisterPeer("device-a", "new-host", nil, newWriter, nil, 20001)
	coordinator.UnregisterPeer("device-a", oldWriter)

	if got := coordinator.GetPeer("device-a"); got != newPeer {
		t.Fatalf("old session removed replacement peer: got %+v, want %+v", got, newPeer)
	}
	coordinator.UnregisterPeer("device-a", newWriter)
	if got := coordinator.GetPeer("device-a"); got != nil {
		t.Fatalf("current session was not removed: %+v", got)
	}
}

func TestConnectRequestRequiresCoordinatorAuthorization(t *testing.T) {
	coordinator := NewSessionCoordinator()
	targetBuffer := &bytes.Buffer{}
	targetWriter := protocol.NewControlWriter(targetBuffer)
	coordinator.RegisterPeer("target-b", "target", nil, targetWriter, nil, 20001)

	controller := &ActivePeer{DeviceID: "controller-a"}
	req := &protocol.ControlMessage{TargetDeviceID: "target-b"}
	resp, err := coordinator.HandleConnectRequest(controller, req, "relay.example.com")
	if err != nil {
		t.Fatalf("handle unauthorized request: %v", err)
	}
	if resp.Type != protocol.MsgTypeError || resp.Code != 403 {
		t.Fatalf("got response %+v, want authorization rejection", resp)
	}
	if targetBuffer.Len() != 0 {
		t.Fatal("unauthorized request notified target")
	}

	coordinator.SetAuthorizer(func(controllerID, targetID string) bool {
		return controllerID == "controller-a" && targetID == "target-b"
	})
	resp, err = coordinator.HandleConnectRequest(controller, req, "relay.example.com")
	if err != nil {
		t.Fatalf("handle authorized request: %v", err)
	}
	if resp.Type != protocol.MsgTypeConnectResponse || resp.SessionID&0x80000000 == 0 {
		t.Fatalf("unexpected authorized response: %+v", resp)
	}
	notify, err := protocol.ReadControlMessage(targetBuffer)
	if err != nil {
		t.Fatalf("read target notification: %v", err)
	}
	if notify.Type != protocol.MsgTypeConnectNotify || notify.SessionID != resp.SessionID {
		t.Fatalf("unexpected target notification: %+v", notify)
	}
}
