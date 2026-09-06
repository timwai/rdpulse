package agent

import (
	"context"
	"errors"
	"log"
	"net"
	"time"

	"rdpulse/internal/punch"
	"rdpulse/internal/ratelimit"
	"rdpulse/internal/tcp"
)

const maxConcurrentDirectTCPHandshakes = 64

func (c *Client) acceptDirectTCP(ctx context.Context, listener *net.TCPListener, sessions *p2pSessionRegistry) error {
	sem := make(chan struct{}, maxConcurrentDirectTCPHandshakes)
	guard := ratelimit.NewIPGuard(8, 30, time.Minute)
	for {
		conn, err := listener.AcceptTCP()
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				return err
			}
		}
		releaseIP, allowed := guard.Acquire(conn.RemoteAddr())
		if !allowed {
			_ = conn.Close()
			continue
		}
		select {
		case sem <- struct{}{}:
			go func() {
				defer func() { <-sem }()
				defer releaseIP()
				c.handleDirectTCP(ctx, conn, sessions)
			}()
		default:
			releaseIP()
			_ = conn.Close()
		}
	}
}

func (c *Client) handleDirectTCP(ctx context.Context, peer net.Conn, sessions *p2pSessionRegistry) {
	defer peer.Close()
	sessionID, err := punch.AcceptTCPPeer(peer, sessions.lookup)
	if err != nil {
		if errors.Is(err, punch.ErrTCPProbe) {
			return
		}
		if !errors.Is(err, net.ErrClosed) {
			log.Printf("[Agent] Rejected direct TCP peer: %v", err)
		}
		return
	}

	target := c.cfg.RDP.Address
	if target == "" {
		target = "127.0.0.1:3389"
	}
	local, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		log.Printf("[Agent] Direct TCP session %d could not reach local RDP: %v", sessionID, err)
		return
	}
	defer local.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tcp.Bridge(local, peer)
	}()
	select {
	case <-ctx.Done():
		_ = local.Close()
		_ = peer.Close()
	case <-done:
	}
}
