package rendezvous

import (
	"context"
	"testing"

	"rdpulse/internal/nat"
)

func TestRendezvousProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := StartServer(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("StartServer failed: %v", err)
	}
	defer srv.Close()

	serverAddr := srv.Addr().String()

	// Probe from client
	cand, err := nat.ProbeReflexiveCandidate(ctx, serverAddr, nil)
	if err != nil {
		t.Fatalf("ProbeReflexiveCandidate failed: %v", err)
	}

	if cand.Protocol != nat.ProtocolUDP || cand.Type != nat.CandidateTypeReflexive {
		t.Fatalf("unexpected candidate attributes: %+v", cand)
	}

	t.Logf("Successfully discovered reflexive candidate: %s", cand.Address)

	// Test LAN candidate discovery
	lanCands, err := nat.DiscoverLANCandidates(13389, 13389)
	if err != nil {
		t.Fatalf("DiscoverLANCandidates failed: %v", err)
	}
	t.Logf("Discovered %d LAN candidates", len(lanCands))
}
