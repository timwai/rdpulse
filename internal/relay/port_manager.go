package relay

import (
	"errors"
	"fmt"
	"sync"

	"rdpulse/internal/storage"
)

var (
	ErrNoAvailablePort = errors.New("no available public ports in configured range")
)

// PortManager handles allocating and preserving public TCP/UDP ports for registered devices
type PortManager struct {
	mu           sync.Mutex
	startPort    uint16
	endPort      uint16
	db           *storage.DB
	deviceToPort map[string]uint16
	portToDevice map[uint16]string
}

// NewPortManager creates a PortManager initialized from DB state
func NewPortManager(startPort, endPort uint16, db *storage.DB) (*PortManager, error) {
	if startPort > endPort || startPort == 0 {
		return nil, fmt.Errorf("invalid port range: %d-%d", startPort, endPort)
	}

	pm := &PortManager{
		startPort:    startPort,
		endPort:      endPort,
		db:           db,
		deviceToPort: make(map[string]uint16),
		portToDevice: make(map[uint16]string),
	}

	// Preload existing assignments from database
	if db != nil {
		agents, err := db.GetAllAgents()
		if err != nil {
			return nil, fmt.Errorf("preload agents for port manager failed: %w", err)
		}
		for _, a := range agents {
			if a.PublicPort >= startPort && a.PublicPort <= endPort {
				pm.deviceToPort[a.DeviceID] = a.PublicPort
				pm.portToDevice[a.PublicPort] = a.DeviceID
			}
		}
	}

	return pm, nil
}

// GetOrAllocatePort returns existing assigned port for device or finds next available port
func (pm *PortManager) GetOrAllocatePort(deviceID string) (uint16, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// 1. Check existing assignment
	if port, exists := pm.deviceToPort[deviceID]; exists {
		return port, nil
	}

	// 2. Allocate lowest free port in range
	for p := uint32(pm.startPort); p <= uint32(pm.endPort); p++ {
		port := uint16(p)
		if _, taken := pm.portToDevice[port]; !taken {
			pm.deviceToPort[deviceID] = port
			pm.portToDevice[port] = deviceID
			return port, nil
		}
	}

	return 0, ErrNoAvailablePort
}

// GetPort returns port assigned to device without allocating
func (pm *PortManager) GetPort(deviceID string) (uint16, bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	port, exists := pm.deviceToPort[deviceID]
	return port, exists
}

// ReleasePort releases the port assigned to device
func (pm *PortManager) ReleasePort(deviceID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if port, exists := pm.deviceToPort[deviceID]; exists {
		delete(pm.deviceToPort, deviceID)
		delete(pm.portToDevice, port)
	}
}

