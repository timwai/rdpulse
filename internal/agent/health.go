package agent

import (
	"context"
	"net"
	"sync/atomic"
	"time"
)

// HealthChecker periodically verifies if local RDP service is listening
type HealthChecker struct {
	rdpAddress string
	interval   time.Duration
	isOnline   atomic.Bool
	onChange   func(online bool)
}

// NewHealthChecker creates a HealthChecker
func NewHealthChecker(rdpAddr string, interval time.Duration, onChange func(online bool)) *HealthChecker {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	if rdpAddr == "" {
		rdpAddr = "127.0.0.1:3389"
	}
	return &HealthChecker{
		rdpAddress: rdpAddr,
		interval:   interval,
		onChange:   onChange,
	}
}

// IsOnline returns whether RDP is currently responding
func (h *HealthChecker) IsOnline() bool {
	return h.isOnline.Load()
}

// CheckOnce checks RDP port accessibility immediately
func (h *HealthChecker) CheckOnce() bool {
	conn, err := net.DialTimeout("tcp", h.rdpAddress, 2*time.Second)
	online := err == nil
	if online {
		_ = conn.Close()
	}

	old := h.isOnline.Swap(online)
	if old != online && h.onChange != nil {
		h.onChange(online)
	}
	return online
}

// Start runs the periodic health check loop
func (h *HealthChecker) Start(ctx context.Context) {
	h.CheckOnce()
	ticker := time.NewTicker(h.interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.CheckOnce()
			}
		}
	}()
}
