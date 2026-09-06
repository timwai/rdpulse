//go:build !windows

package service

import (
	"context"
	"errors"
)

const (
	ServiceName = "RDPRelayAgent"
	DisplayName = "RDP Relay Agent"
	Description = "High-Performance QUIC Reverse Tunnel Agent for Microsoft RDP"
)

func IsWindowsService() bool {
	return false
}

func RunService(runFn func(ctx context.Context) error) error {
	return runFn(context.Background())
}

func Install(configPath string) error {
	return errors.New("windows service management only supported on windows")
}

func Uninstall() error {
	return errors.New("windows service management only supported on windows")
}

func Start() error {
	return errors.New("windows service management only supported on windows")
}

func Stop() error {
	return errors.New("windows service management only supported on windows")
}

func Status() (string, error) {
	return "Not Supported", errors.New("windows service management only supported on windows")
}
