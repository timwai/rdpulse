package agent

import (
	"context"
	"log"
	"time"

	"rdpulse/internal/nat"
	"rdpulse/internal/protocol"
)

const candidateRefreshInterval = 15 * time.Second

func (c *Client) refreshControlledCandidates(ctx context.Context, writer *protocol.ControlWriter, sessions *p2pSessionRegistry, udpPort, tcpPort int, initial []protocol.CandidateInfo) {
	previous := filterLANCandidates(initial)
	ticker := time.NewTicker(candidateRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			candidates, err := nat.DiscoverLANCandidates(udpPort, tcpPort)
			if err != nil || candidateSetsEqual(previous, candidates) {
				continue
			}
			previous = candidates
			log.Printf("[Agent] 局域网候选已变化，向控制端同步: %s", protocol.FormatCandidates(candidates))
			for _, route := range sessions.routes() {
				_ = writer.WriteMessage(&protocol.ControlMessage{
					Type:           protocol.MsgTypeCandidateExchange,
					DeviceID:       c.cfg.Device.ID,
					TargetDeviceID: route.controller,
					SessionID:      route.sessionID,
					Candidates:     candidates,
				})
			}
		}
	}
}

func (c *Client) refreshControllerCandidates(ctx context.Context, writer *protocol.ControlWriter, targetID string, sessionID uint32, udpPort int, initial []protocol.CandidateInfo) {
	previous := filterLANCandidates(initial)
	ticker := time.NewTicker(candidateRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			candidates, err := nat.DiscoverLANCandidates(udpPort, 0)
			if err != nil || candidateSetsEqual(previous, candidates) {
				continue
			}
			previous = candidates
			log.Printf("[Controller] 局域网候选已变化，向目标同步: %s", protocol.FormatCandidates(candidates))
			_ = writer.WriteMessage(&protocol.ControlMessage{
				Type:           protocol.MsgTypeCandidateExchange,
				DeviceID:       c.cfg.Device.ID,
				TargetDeviceID: targetID,
				SessionID:      sessionID,
				Candidates:     candidates,
			})
		}
	}
}

func filterLANCandidates(candidates []protocol.CandidateInfo) []protocol.CandidateInfo {
	filtered := make([]protocol.CandidateInfo, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Type == nat.CandidateTypeLAN {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func candidateSetsEqual(left, right []protocol.CandidateInfo) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[protocol.CandidateInfo]int, len(left))
	for _, candidate := range left {
		counts[candidate]++
	}
	for _, candidate := range right {
		if counts[candidate] == 0 {
			return false
		}
		counts[candidate]--
	}
	return true
}
