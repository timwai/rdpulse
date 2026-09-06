package gui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"rdpulse/internal/config"
	"gopkg.in/yaml.v3"
)

func TestAppLogWriterAndHistory(t *testing.T) {
	app := &App{}
	writer := &uiLogWriter{app: app}

	testMsg := "2026/09/05 20:00:00 [Test] Hello from test\n"
	n, err := writer.Write([]byte(testMsg))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}
	if n != len(testMsg) {
		t.Fatalf("expected written len %d, got %d", len(testMsg), n)
	}

	app.logMu.Lock()
	defer app.logMu.Unlock()
	if len(app.logHistory) != 1 {
		t.Fatalf("expected 1 log history entry, got %d", len(app.logHistory))
	}
	if app.logHistory[0] != "2026/09/05 20:00:00 [Test] Hello from test" {
		t.Fatalf("unexpected log entry: %s", app.logHistory[0])
	}

	// Verify JSON serialization
	data, err := json.Marshal(app.logHistory)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	var deserialized []string
	if err := json.Unmarshal(data, &deserialized); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if len(deserialized) != 1 || deserialized[0] != app.logHistory[0] {
		t.Fatalf("deserialization mismatch: %v", deserialized)
	}
}

func TestAppTargetAndControllerState(t *testing.T) {
	app := &App{}
	if app.isTargetRunning() {
		t.Fatalf("expected target not running initially")
	}
	if app.isControllerRunning() {
		t.Fatalf("expected controller not running initially")
	}

	app.targetMu.Lock()
	app.targetRunning = true
	app.targetMu.Unlock()
	if !app.isTargetRunning() {
		t.Fatalf("expected target running")
	}

	app.ctrlMu.Lock()
	app.controllerRunning = true
	app.ctrlMu.Unlock()
	if !app.isControllerRunning() {
		t.Fatalf("expected controller running")
	}
}

func TestAppControllerConnectFailure(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "agent.yaml")

	cfg := config.DefaultAgentConfig()
	cfg.Server.Address = "127.0.0.1:59999" // Non-existent server
	cfg.Server.TLSAddress = "127.0.0.1:59999"
	cfg.Server.QUICDialTimeout = 200 * time.Millisecond
	cfg.Device.ID = "test-client"
	cfg.Device.Secret = "12345678901234567890123456789012"
	cfg.Transport.DisableP2P = true

	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal cfg failed: %v", err)
	}
	if err := os.WriteFile(cfgPath, data, 0644); err != nil {
		t.Fatalf("write cfg failed: %v", err)
	}

	app := &App{
		configPath: cfgPath,
		cfg:        cfg,
	}

	// onControllerConnect should synchronously return error when target/relay is unreachable
	err = app.onControllerConnect("target-xyz", "127.0.0.1:13389", false, true)
	if err == nil {
		t.Fatalf("expected connection error for unreachable server, got nil")
	}

	// Ensure state was cleaned up
	if app.isControllerRunning() {
		t.Fatalf("expected controllerRunning to be false after failure")
	}
}

func TestAppTargetStartFailure(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "agent.yaml")

	cfg := config.DefaultAgentConfig()
	cfg.Server.Address = "127.0.0.1:59999"
	cfg.Server.TLSAddress = "127.0.0.1:59999"
	cfg.Server.QUICDialTimeout = 200 * time.Millisecond
	cfg.Device.ID = "test-target"
	cfg.Device.Secret = "12345678901234567890123456789012"

	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal cfg failed: %v", err)
	}
	if err := os.WriteFile(cfgPath, data, 0644); err != nil {
		t.Fatalf("write cfg failed: %v", err)
	}

	app := &App{
		configPath: cfgPath,
		cfg:        cfg,
	}

	// onTargetStart should return error on unreachable server rather than falsely claiming running
	err = app.onTargetStart()
	if err == nil {
		t.Fatalf("expected target start error for unreachable server, got nil")
	}
	if app.isTargetRunning() {
		t.Fatalf("expected targetRunning to be false after failure")
	}
}
