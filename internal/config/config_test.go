package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestConfig(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	return path
}

func TestLoadAgentConfigRejectsWeakCredentials(t *testing.T) {
	path := writeTestConfig(t, "agent.yaml", `
server:
  address: relay.example.com:443
device:
  id: device-a
  secret: short
`)
	_, err := LoadAgentConfig(path)
	if err == nil || !strings.Contains(err.Error(), "at least 32 bytes") {
		t.Fatalf("got error %v, want weak-secret rejection", err)
	}
}

func TestLoadRelayConfigRequiresScopedEnrollmentInvitations(t *testing.T) {
	weakTokenPath := writeTestConfig(t, "weak-token.yaml", `
rdp:
  publicHost: relay.example.com
security:
  enrollmentToken: short
`)
	if _, err := LoadRelayConfig(weakTokenPath); err == nil || !strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("got error %v, want global enrollment-token rejection", err)
	}

	weakInvitePath := writeTestConfig(t, "weak-invite.yaml", `
rdp:
  publicHost: relay.example.com
security:
  enrollmentInvitations:
    - deviceID: device-a
      token: short
      expiresAt: "2099-01-01T00:00:00Z"
`)
	if _, err := LoadRelayConfig(weakInvitePath); err == nil || !strings.Contains(err.Error(), "32-byte token") {
		t.Fatalf("got error %v, want weak invitation rejection", err)
	}

	missingHostPath := writeTestConfig(t, "missing-host.yaml", `
security:
  defaultPolicy: deny
`)
	if _, err := LoadRelayConfig(missingHostPath); err == nil || !strings.Contains(err.Error(), "publicHost") {
		t.Fatalf("got error %v, want missing public-host rejection", err)
	}
}

func TestLoadRelayConfigAllowsExplicitEphemeralDevelopmentCertificate(t *testing.T) {
	path := writeTestConfig(t, "relay.yaml", `
server:
  quic:
    allowEphemeralCertificate: true
rdp:
  publicHost: relay.example.com
security:
  enrollmentInvitations:
    - deviceID: device-a
      token: invitation-token-at-least-32-bytes-long
      expiresAt: "2099-01-01T00:00:00Z"
`)
	cfg, err := LoadRelayConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Security.EnrollmentInvitations) != 1 {
		t.Fatalf("invitation not loaded: %+v", cfg.Security.EnrollmentInvitations)
	}
}

func TestLoadRelayConfigRejectsImplicitEphemeralCertificate(t *testing.T) {
	path := writeTestConfig(t, "relay.yaml", `
rdp:
  publicHost: relay.example.com
`)
	if _, err := LoadRelayConfig(path); err == nil || !strings.Contains(err.Error(), "certificate is required") {
		t.Fatalf("got error %v, want missing-certificate rejection", err)
	}
}

func TestSyncInvitationsToConfigFile(t *testing.T) {
	initialYaml := `# Header comment
server:
  quic:
    allowEphemeralCertificate: true
rdp:
  publicHost: relay.example.com
security:
  # Security comment
  defaultPolicy: deny
  enrollmentInvitations: []
`
	path := writeTestConfig(t, "relay-sync.yaml", initialYaml)

	expires := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	invs := []EnrollmentInvitation{
		{
			DeviceID:  "device-synced-1",
			Token:     "12345678901234567890123456789012",
			ExpiresAt: expires,
		},
		{
			DeviceID:  "device-synced-2",
			Token:     "abcdefabcdefabcdefabcdefabcdefab",
			ExpiresAt: expires,
		},
	}

	if err := SyncInvitationsToConfigFile(path, invs); err != nil {
		t.Fatalf("SyncInvitationsToConfigFile failed: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	contentStr := string(content)

	// Verify comments preserved
	if !strings.Contains(contentStr, "# Header comment") {
		t.Errorf("Header comment lost")
	}
	if !strings.Contains(contentStr, "# Security comment") {
		t.Errorf("Security comment lost")
	}

	// Verify reload
	cfg, err := LoadRelayConfig(path)
	if err != nil {
		t.Fatalf("reload config failed: %v", err)
	}
	if len(cfg.Security.EnrollmentInvitations) != 2 {
		t.Fatalf("expected 2 invitations, got %d", len(cfg.Security.EnrollmentInvitations))
	}
	if cfg.Security.EnrollmentInvitations[0].DeviceID != "device-synced-1" {
		t.Errorf("expected device-synced-1, got %s", cfg.Security.EnrollmentInvitations[0].DeviceID)
	}

	// Test clearing to empty []
	if err := SyncInvitationsToConfigFile(path, []EnrollmentInvitation{}); err != nil {
		t.Fatalf("clear invitations failed: %v", err)
	}
	cfg, err = LoadRelayConfig(path)
	if err != nil {
		t.Fatalf("reload empty config failed: %v", err)
	}
	if len(cfg.Security.EnrollmentInvitations) != 0 {
		t.Fatalf("expected 0 invitations, got %d", len(cfg.Security.EnrollmentInvitations))
	}
}

func TestSyncAccessPolicyToConfigFile(t *testing.T) {
	initialYaml := `# Header comment
server:
  quic:
    allowEphemeralCertificate: true
rdp:
  publicHost: relay.example.com
security:
  # Security policy comment
  defaultPolicy: deny
  allowAllControllerTargets: false
  controllerAccess: {}
`
	path := writeTestConfig(t, "relay-access-sync.yaml", initialYaml)

	accessMap := map[string][]string{
		"target-pc-1": {"controller-admin", "controller-user"},
		"target-pc-2": {"*"},
	}

	if err := SyncAccessPolicyToConfigFile(path, true, accessMap); err != nil {
		t.Fatalf("SyncAccessPolicyToConfigFile failed: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	contentStr := string(content)

	// Verify comments preserved
	if !strings.Contains(contentStr, "# Header comment") {
		t.Errorf("Header comment lost")
	}
	if !strings.Contains(contentStr, "# Security policy comment") {
		t.Errorf("Security policy comment lost")
	}

	// Verify reload
	cfg, err := LoadRelayConfig(path)
	if err != nil {
		t.Fatalf("reload config failed: %v", err)
	}
	if !cfg.Security.AllowAllControllerTargets {
		t.Errorf("expected AllowAllControllerTargets to be true")
	}
	if len(cfg.Security.ControllerAccess) != 2 {
		t.Fatalf("expected 2 targets in ControllerAccess, got %d", len(cfg.Security.ControllerAccess))
	}
	if len(cfg.Security.ControllerAccess["target-pc-1"]) != 2 {
		t.Errorf("expected 2 controllers for target-pc-1, got %v", cfg.Security.ControllerAccess["target-pc-1"])
	}

	// Test clearing back to false and empty map
	if err := SyncAccessPolicyToConfigFile(path, false, nil); err != nil {
		t.Fatalf("SyncAccessPolicyToConfigFile clear failed: %v", err)
	}
	cfg, err = LoadRelayConfig(path)
	if err != nil {
		t.Fatalf("reload cleared config failed: %v", err)
	}
	if cfg.Security.AllowAllControllerTargets {
		t.Errorf("expected AllowAllControllerTargets to be false")
	}
	if len(cfg.Security.ControllerAccess) != 0 {
		t.Errorf("expected empty ControllerAccess, got %v", cfg.Security.ControllerAccess)
	}
}

func TestSyncServerSettingsToConfigFile(t *testing.T) {
	initialYaml := `# Server settings test
server:
  quic:
    allowEphemeralCertificate: true
rdp:
  publicHost: old.example.com
  portRange:
    start: 3389
    end: 3389
security:
  defaultPolicy: deny
web:
  token: ""
`
	path := writeTestConfig(t, "relay-settings-sync.yaml", initialYaml)

	newHost := "new.example.com"
	newStart := 20000
	newEnd := 20100
	newPolicy := "allow"
	newToken := "my-secret-token"

	err := SyncServerSettingsToConfigFile(path, ServerSettingsUpdate{
		PublicHost:     &newHost,
		PortRangeStart: &newStart,
		PortRangeEnd:   &newEnd,
		DefaultPolicy:  &newPolicy,
		WebToken:       &newToken,
	})
	if err != nil {
		t.Fatalf("SyncServerSettingsToConfigFile failed: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "# Server settings test") {
		t.Errorf("Comment lost during settings sync")
	}

	cfg, err := LoadRelayConfig(path)
	if err != nil {
		t.Fatalf("reload config failed: %v", err)
	}
	if cfg.RDP.PublicHost != "new.example.com" {
		t.Errorf("expected new.example.com, got %s", cfg.RDP.PublicHost)
	}
	if cfg.RDP.PortRange.Start != 20000 || cfg.RDP.PortRange.End != 20100 {
		t.Errorf("unexpected port range: %d-%d", cfg.RDP.PortRange.Start, cfg.RDP.PortRange.End)
	}
	if cfg.Security.DefaultPolicy != "allow" {
		t.Errorf("expected policy allow, got %s", cfg.Security.DefaultPolicy)
	}
	if cfg.Web.Token != "my-secret-token" {
		t.Errorf("expected token my-secret-token, got %s", cfg.Web.Token)
	}
}

func TestLoadAgentConfigAppliesDefaultsToZeroValues(t *testing.T) {
	path := writeTestConfig(t, "zero-agent.yaml", `
server:
  address: relay.example.com:443
  quicDialTimeout: 0s
device:
  id: device-test
  secret: 0123456789abcdef0123456789abcdef
heartbeat:
  interval: 0s
reconnect:
  maxInterval: 0s
udp:
  sessionIdleTimeout: 0s
  reassemblyTimeout: 0s
  maxFragments: 0
transport:
  datagramPayload: 0
`)
	cfg, err := LoadAgentConfig(path)
	if err != nil {
		t.Fatalf("LoadAgentConfig failed: %v", err)
	}
	if cfg.Heartbeat.Interval <= 0 {
		t.Errorf("expected positive heartbeat interval, got %v", cfg.Heartbeat.Interval)
	}
	if cfg.Reconnect.MaxInterval <= 0 {
		t.Errorf("expected positive reconnect maxInterval, got %v", cfg.Reconnect.MaxInterval)
	}
	if cfg.Server.QUICDialTimeout <= 0 {
		t.Errorf("expected positive quicDialTimeout, got %v", cfg.Server.QUICDialTimeout)
	}
	if cfg.UDP.SessionIdleTimeout <= 0 {
		t.Errorf("expected positive sessionIdleTimeout, got %v", cfg.UDP.SessionIdleTimeout)
	}
	if cfg.UDP.ReassemblyTimeout <= 0 {
		t.Errorf("expected positive reassemblyTimeout, got %v", cfg.UDP.ReassemblyTimeout)
	}
	if cfg.UDP.MaxFragments <= 0 {
		t.Errorf("expected positive maxFragments, got %d", cfg.UDP.MaxFragments)
	}
	if cfg.Transport.DatagramPayload <= 0 {
		t.Errorf("expected positive datagramPayload, got %d", cfg.Transport.DatagramPayload)
	}
	if cfg.RDP.Address != "127.0.0.1:3389" {
		t.Errorf("expected default RDP address 127.0.0.1:3389, got %s", cfg.RDP.Address)
	}
}

func TestLoadAgentConfigPersistsGUITheme(t *testing.T) {
	path := writeTestConfig(t, "theme-agent.yaml", `
server:
  address: relay.example.com:443
device:
  id: device-test
  secret: 0123456789abcdef0123456789abcdef
gui:
  theme: light
`)
	cfg, err := LoadAgentConfig(path)
	if err != nil {
		t.Fatalf("LoadAgentConfig failed: %v", err)
	}
	if cfg.GUI.Theme != "light" {
		t.Fatalf("theme = %q, want light", cfg.GUI.Theme)
	}

	emptyPath := writeTestConfig(t, "theme-default.yaml", `
server:
  address: relay.example.com:443
device:
  id: device-test
  secret: 0123456789abcdef0123456789abcdef
`)
	empty, err := LoadAgentConfig(emptyPath)
	if err != nil {
		t.Fatalf("LoadAgentConfig empty theme: %v", err)
	}
	if empty.GUI.Theme != "dark" {
		t.Fatalf("empty theme = %q, want dark", empty.GUI.Theme)
	}

	cfg.GUI.Theme = "not-a-theme"
	cfg.SetDefaults()
	if cfg.GUI.Theme != "dark" {
		t.Fatalf("invalid theme normalized to %q, want dark", cfg.GUI.Theme)
	}
}

