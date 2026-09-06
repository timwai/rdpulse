package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"rdpulse/internal/transport"
	"rdpulse/internal/transport/quicgo"
	"rdpulse/internal/transport/tlsmux"
)

const quicDialTimeout = 4 * time.Second

func (c *Client) dialRelayTransport(ctx context.Context) (transport.Transport, string, error) {
	tlsConfig, err := c.buildTLSConfig()
	if err != nil {
		return nil, "", fmt.Errorf("build tls config failed: %w", err)
	}
	var quicErr error
	if !c.cfg.Server.DisableQUIC {
		timeout := c.cfg.Server.QUICDialTimeout
		if timeout <= 0 {
			timeout = quicDialTimeout
		}
		quicCtx, cancel := context.WithTimeout(ctx, timeout)
		quicTransport, err := quicgo.Dial(quicCtx, c.cfg.Server.Address, tlsConfig, quicgo.DefaultQUICConfig())
		cancel()
		if err == nil {
			return quicTransport, "QUIC", nil
		}
		quicErr = err
	} else {
		quicErr = errors.New("QUIC disabled by configuration")
	}
	if c.cfg.Server.DisableTLSFallback {
		return nil, "", fmt.Errorf("dial QUIC: %w", quicErr)
	}
	tlsAddress := c.cfg.Server.TLSAddress
	if tlsAddress == "" {
		tlsAddress = c.cfg.Server.Address
	}
	log.Printf("[Agent] QUIC unavailable (%v); trying TLS/TCP fallback at %s", quicErr, tlsAddress)
	tlsTransport, tlsErr := tlsmux.Dial(ctx, tlsAddress, tlsConfig)
	if tlsErr != nil {
		return nil, "", errors.Join(fmt.Errorf("dial QUIC: %w", quicErr), fmt.Errorf("dial TLS/TCP: %w", tlsErr))
	}
	return tlsTransport, "TLS/TCP", nil
}
