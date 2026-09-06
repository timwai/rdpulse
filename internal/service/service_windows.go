//go:build windows

package service

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	ServiceName = "RDPRelayAgent"
	DisplayName = "RDP Relay Agent"
	Description = "High-Performance QUIC Reverse Tunnel Agent for Microsoft RDP"
)

type agentService struct {
	runFn func(ctx context.Context) error
}

func (s *agentService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.runFn(ctx)
	}()

	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

loop:
	for {
		select {
		case err := <-errCh:
			if err != nil {
				log.Printf("Service execution failed: %v", err)
			}
			break loop
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				// Wait for runFn to finish gracefully up to 10s
				select {
				case <-errCh:
				case <-time.After(10 * time.Second):
					log.Printf("Service graceful stop timed out after 10s")
				}
				break loop
			default:
				log.Printf("Unexpected control request #%d", c)
			}
		}
	}

	changes <- svc.Status{State: svc.Stopped}
	return false, 0
}

// IsWindowsService reports whether the process is executing as a Windows service
func IsWindowsService() bool {
	isService, err := svc.IsWindowsService()
	return err == nil && isService
}

// RunService runs the application as a Windows service if interactive is false, otherwise runs in foreground
func RunService(runFn func(ctx context.Context) error) error {
	if IsWindowsService() {
		return svc.Run(ServiceName, &agentService{runFn: runFn})
	}

	// Foreground interactive run
	return runFn(context.Background())
}

// Install registers the service in Windows SCM
func Install(configPath string) error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	exePath, err = filepath.Abs(exePath)
	if err != nil {
		return err
	}
	if configPath != "" {
		configPath, err = filepath.Abs(configPath)
		if err != nil {
			return fmt.Errorf("resolve config path failed: %w", err)
		}
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager failed: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err == nil {
		s.Close()
		return fmt.Errorf("service %s already exists", ServiceName)
	}

	args := []string{"service"}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}

	// Double-quote binary path to prevent CWE-428 unquoted service path vulnerability
	binPath := fmt.Sprintf("\"%s\"", exePath)
	s, err = m.CreateService(ServiceName, binPath, mgr.Config{
		DisplayName: DisplayName,
		Description: Description,
		StartType:   mgr.StartAutomatic,
	}, args...)
	if err != nil {
		return fmt.Errorf("create service failed: %w", err)
	}
	defer s.Close()

	return nil
}

// Uninstall removes the service from Windows SCM
func Uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("service %s not found: %w", ServiceName, err)
	}
	defer s.Close()

	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service failed: %w", err)
	}
	return nil
}

// Start starts the service
func Start() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("open service failed: %w", err)
	}
	defer s.Close()

	if err := s.Start(); err != nil {
		return fmt.Errorf("start service failed: %w", err)
	}
	return nil
}

// Stop stops the service
func Stop() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("open service failed: %w", err)
	}
	defer s.Close()

	status, err := s.Control(svc.Stop)
	if err != nil {
		return fmt.Errorf("send stop control failed: %w", err)
	}

	timeout := time.Now().Add(10 * time.Second)
	for status.State != svc.Stopped {
		if time.Now().After(timeout) {
			return fmt.Errorf("service stop timed out")
		}
		time.Sleep(300 * time.Millisecond)
		status, err = s.Query()
		if err != nil {
			return fmt.Errorf("query service status failed: %w", err)
		}
	}
	return nil
}

// Status returns current service status description
func Status() (string, error) {
	m, err := mgr.Connect()
	if err != nil {
		return "", err
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return "Not Installed", nil
	}
	defer s.Close()

	status, err := s.Query()
	if err != nil {
		return "", err
	}

	switch status.State {
	case svc.Stopped:
		return "Stopped", nil
	case svc.StartPending:
		return "Start Pending", nil
	case svc.StopPending:
		return "Stop Pending", nil
	case svc.Running:
		return "Running", nil
	default:
		return fmt.Sprintf("State (%d)", status.State), nil
	}
}
