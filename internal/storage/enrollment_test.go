package storage

import (
	"errors"
	"testing"
	"time"
)

func TestEnrollmentInvitationIsScopedExpiringAndOneTime(t *testing.T) {
	db, err := Open(t.TempDir() + "/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	token := "one-time-enrollment-token-at-least-32-bytes"
	if err := db.UpsertEnrollmentInvitation("device-a", token, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ValidateEnrollmentInvitation("device-b", token, now); err != nil || ok {
		t.Fatalf("invitation crossed device boundary: ok=%v err=%v", ok, err)
	}
	record := &AgentRecord{DeviceID: "device-a", SecretHash: "hash", Enabled: true}
	if err := db.CreateAgentWithEnrollment(record, token, now); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateAgentWithEnrollment(&AgentRecord{DeviceID: "device-a", SecretHash: "replacement", Enabled: true}, token, now); !errors.Is(err, ErrEnrollmentInvalid) {
		t.Fatalf("reused invitation error=%v", err)
	}
	if err := db.UpsertEnrollmentInvitation("device-a", token, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ValidateEnrollmentInvitation("device-a", token, now); err != nil || ok {
		t.Fatalf("restart with same token reset consumption: ok=%v err=%v", ok, err)
	}
	rotated := "rotated-enrollment-token-at-least-32-bytes"
	if err := db.UpsertEnrollmentInvitation("device-a", rotated, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ValidateEnrollmentInvitation("device-a", rotated, now); err != nil || !ok {
		t.Fatalf("rotated invitation unavailable: ok=%v err=%v", ok, err)
	}

	expired := "expired-enrollment-token-at-least-32-bytes"
	if err := db.UpsertEnrollmentInvitation("device-expired", expired, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ValidateEnrollmentInvitation("device-expired", expired, now); err != nil || ok {
		t.Fatalf("expired invitation accepted: ok=%v err=%v", ok, err)
	}
}

func TestManagementMethods(t *testing.T) {
	db, err := Open(t.TempDir() + "/relay_mgmt.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()

	// 1. Test invitations list and delete
	token := "management-test-token-at-least-32-bytes"
	if err := db.UpsertEnrollmentInvitation("device-mgmt", token, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	invs, err := db.ListEnrollmentInvitations()
	if err != nil || len(invs) != 1 || invs[0].DeviceID != "device-mgmt" {
		t.Fatalf("expected 1 invitation, got %v, err=%v", invs, err)
	}
	if err := db.DeleteEnrollmentInvitation("device-mgmt"); err != nil {
		t.Fatal(err)
	}
	invs, err = db.ListEnrollmentInvitations()
	if err != nil || len(invs) != 0 {
		t.Fatalf("expected 0 invitations after delete, got %v", invs)
	}

	// 2. Test agent enable/disable and delete
	agent := &AgentRecord{
		DeviceID:   "agent-1",
		Hostname:   "host-1",
		SecretHash: "hash1",
		PublicPort: 20001,
		Enabled:    true,
	}
	if err := db.UpsertAgent(agent); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAgentEnabled("agent-1", false); err != nil {
		t.Fatal(err)
	}
	fetched, err := db.GetAgentByDeviceID("agent-1")
	if err != nil || fetched == nil || fetched.Enabled {
		t.Fatalf("expected agent-1 to be disabled, got %v", fetched)
	}
	if err := db.DeleteAgent("agent-1"); err != nil {
		t.Fatal(err)
	}
	fetched, err = db.GetAgentByDeviceID("agent-1")
	if err != nil || fetched != nil {
		t.Fatalf("expected agent-1 to be deleted, got %v", fetched)
	}
}
