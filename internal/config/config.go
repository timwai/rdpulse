package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ServerSettingsUpdate represents editable server configuration settings
type ServerSettingsUpdate struct {
	PublicHost       *string `json:"publicHost,omitempty"`
	PortRangeStart   *int    `json:"portRangeStart,omitempty"`
	PortRangeEnd     *int    `json:"portRangeEnd,omitempty"`
	DefaultPolicy    *string `json:"defaultPolicy,omitempty"`
	WebToken         *string `json:"webToken,omitempty"`
	QuicListen       *string `json:"quicListen,omitempty"`
	TlsListen        *string `json:"tlsListen,omitempty"`
	TlsDisabled      *bool   `json:"tlsDisabled,omitempty"`
	RendezvousListen *string `json:"rendezvousListen,omitempty"`
}

type EnrollmentInvitation struct {
	DeviceID  string    `yaml:"deviceID"`
	Token     string    `yaml:"token"`
	ExpiresAt time.Time `yaml:"expiresAt"`
}

// RelayConfig holds all configuration parameters for the Relay server
type RelayConfig struct {
	Server struct {
		QUIC struct {
			Listen                    string `yaml:"listen"`
			CertFile                  string `yaml:"certFile"`
			KeyFile                   string `yaml:"keyFile"`
			AllowEphemeralCertificate bool   `yaml:"allowEphemeralCertificate"`
		} `yaml:"quic"`
		TLS struct {
			Listen   string `yaml:"listen"`
			Disabled bool   `yaml:"disabled"`
		} `yaml:"tls"`
		Rendezvous struct {
			Listen string `yaml:"listen"`
		} `yaml:"rendezvous"`
	} `yaml:"server"`

	RDP struct {
		PublicHost string `yaml:"publicHost"`
		PortRange  struct {
			Start uint16 `yaml:"start"`
			End   uint16 `yaml:"end"`
		} `yaml:"portRange"`
	} `yaml:"rdp"`

	UDP struct {
		DatagramPayload      int           `yaml:"datagramPayload"`
		SessionIdleTimeout   time.Duration `yaml:"sessionIdleTimeout"`
		ReassemblyTimeout    time.Duration `yaml:"reassemblyTimeout"`
		MaxFragments         int           `yaml:"maxFragments"`
		MaxSessionsPerAgent  int           `yaml:"maxSessionsPerAgent"`
		MaxSessionsPerIP     int           `yaml:"maxSessionsPerIP"`
		MaxSessionCreateRate int           `yaml:"maxSessionCreateRate"`
	} `yaml:"udp"`

	Security struct {
		DefaultPolicy             string                 `yaml:"defaultPolicy"` // "allow" or "deny"
		Allow                     []string               `yaml:"allow"`         // CIDR blocks
		EnrollmentToken           string                 `yaml:"enrollmentToken"`
		EnrollmentInvitations     []EnrollmentInvitation `yaml:"enrollmentInvitations"`
		AllowAllControllerTargets bool                   `yaml:"allowAllControllerTargets"`
		ControllerAccess          map[string][]string    `yaml:"controllerAccess"`
		Limits                    struct {
			MaxConnectionsPerIP        int `yaml:"maxConnectionsPerIP"`
			MaxConnectionRatePerMinute int `yaml:"maxConnectionRatePerMinute"`
			MaxEnrollmentRatePerMinute int `yaml:"maxEnrollmentRatePerMinute"`
			MaxPublicTCPPerIP          int `yaml:"maxPublicTCPPerIP"`
			MaxPublicTCPRatePerSecond  int `yaml:"maxPublicTCPRatePerSecond"`
		} `yaml:"limits"`
	} `yaml:"security"`

	Storage struct {
		Type string `yaml:"type"` // "sqlite"
		Path string `yaml:"path"`
	} `yaml:"storage"`

	Metrics struct {
		Listen string `yaml:"listen"`
	} `yaml:"metrics"`

	Web struct {
		Listen   string `yaml:"listen"`
		Disabled bool   `yaml:"disabled"`
		Token    string `yaml:"token"`
	} `yaml:"web"`

	Log struct {
		Level string `yaml:"level"`
	} `yaml:"log"`
}

// AgentConfig holds all configuration parameters for the Windows Agent
type AgentConfig struct {
	Server struct {
		Address            string        `yaml:"address" json:"address"`
		TLSAddress         string        `yaml:"tlsAddress" json:"tlsAddress"`
		RendezvousAddress  string        `yaml:"rendezvousAddress" json:"rendezvousAddress"`
		CACert             string        `yaml:"caCert" json:"caCert"`
		InsecureSkipVerify bool          `yaml:"insecureSkipVerify" json:"insecureSkipVerify"` // For dev testing
		DisableQUIC        bool          `yaml:"disableQUIC" json:"disableQUIC"`
		QUICDialTimeout    time.Duration `yaml:"quicDialTimeout" json:"quicDialTimeout"`
		DisableTLSFallback bool          `yaml:"disableTLSFallback" json:"disableTLSFallback"`
	} `yaml:"server" json:"server"`

	Device struct {
		ID              string `yaml:"id" json:"id"`
		Secret          string `yaml:"secret" json:"secret"`
		EnrollmentToken string `yaml:"enrollmentToken" json:"enrollmentToken"`
	} `yaml:"device" json:"device"`

	RDP struct {
		Address string `yaml:"address" json:"address"` // default 127.0.0.1:3389
	} `yaml:"rdp" json:"rdp"`

	Transport struct {
		DatagramPayload int  `yaml:"datagramPayload" json:"datagramPayload"`
		DisableP2P      bool `yaml:"disableP2P" json:"disableP2P"`
	} `yaml:"transport" json:"transport"`

	UDP struct {
		SessionIdleTimeout time.Duration `yaml:"sessionIdleTimeout" json:"sessionIdleTimeout"`
		ReassemblyTimeout  time.Duration `yaml:"reassemblyTimeout" json:"reassemblyTimeout"`
		MaxFragments       int           `yaml:"maxFragments" json:"maxFragments"`
	} `yaml:"udp" json:"udp"`

	Heartbeat struct {
		Interval time.Duration `yaml:"interval" json:"interval"`
	} `yaml:"heartbeat" json:"heartbeat"`

	Reconnect struct {
		MaxInterval time.Duration `yaml:"maxInterval" json:"maxInterval"`
	} `yaml:"reconnect" json:"reconnect"`

	Log struct {
		Level string `yaml:"level" json:"level"`
		Path  string `yaml:"path" json:"path"`
	} `yaml:"log" json:"log"`

	GUI struct {
		AutoStartTarget bool   `yaml:"autoStartTarget" json:"autoStartTarget"`
		MinimizeToTray  bool   `yaml:"minimizeToTray" json:"minimizeToTray"`
		Theme           string `yaml:"theme" json:"theme"`
	} `yaml:"gui" json:"gui"`
}

// LoadRelayConfig reads and parses a YAML configuration file for Relay
func LoadRelayConfig(path string) (*RelayConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read relay config file failed: %w", err)
	}

	cfg := &RelayConfig{}
	// Defaults
	cfg.Server.QUIC.Listen = ":443"
	cfg.Server.TLS.Listen = ":443"
	cfg.Server.Rendezvous.Listen = ":21116"
	cfg.RDP.PublicHost = ""
	cfg.RDP.PortRange.Start = 20000
	cfg.RDP.PortRange.End = 39999
	cfg.UDP.DatagramPayload = 1150
	cfg.UDP.SessionIdleTimeout = 60 * time.Second
	cfg.UDP.ReassemblyTimeout = 100 * time.Millisecond
	cfg.UDP.MaxFragments = 64
	cfg.UDP.MaxSessionsPerAgent = 256
	cfg.UDP.MaxSessionsPerIP = 32
	cfg.UDP.MaxSessionCreateRate = 50
	cfg.Security.DefaultPolicy = "deny"
	cfg.Security.ControllerAccess = make(map[string][]string)
	cfg.Security.Limits.MaxConnectionsPerIP = 16
	cfg.Security.Limits.MaxConnectionRatePerMinute = 60
	cfg.Security.Limits.MaxEnrollmentRatePerMinute = 5
	cfg.Security.Limits.MaxPublicTCPPerIP = 32
	cfg.Security.Limits.MaxPublicTCPRatePerSecond = 20
	cfg.Storage.Type = "sqlite"
	cfg.Storage.Path = "data/relay.db"
	cfg.Metrics.Listen = "127.0.0.1:9090"
	cfg.Web.Listen = "127.0.0.1:8080"
	cfg.Web.Disabled = false
	cfg.Web.Token = ""
	cfg.Log.Level = "info"

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse relay config yaml failed: %w", err)
	}
	if cfg.Security.EnrollmentToken != "" {
		return nil, fmt.Errorf("security.enrollmentToken is no longer supported; use scoped, expiring enrollmentInvitations")
	}
	invitationDevices := make(map[string]struct{}, len(cfg.Security.EnrollmentInvitations))
	for i, invitation := range cfg.Security.EnrollmentInvitations {
		if strings.TrimSpace(invitation.DeviceID) == "" || len(invitation.Token) < 32 || invitation.ExpiresAt.IsZero() {
			return nil, fmt.Errorf("security.enrollmentInvitations[%d] requires deviceID, a 32-byte token, and expiresAt", i)
		}
		if !invitation.ExpiresAt.After(time.Now()) {
			return nil, fmt.Errorf("security.enrollmentInvitations[%d] is expired", i)
		}
		if _, exists := invitationDevices[invitation.DeviceID]; exists {
			return nil, fmt.Errorf("duplicate enrollment invitation for device %s", invitation.DeviceID)
		}
		invitationDevices[invitation.DeviceID] = struct{}{}
	}
	if strings.TrimSpace(cfg.RDP.PublicHost) == "" {
		return nil, fmt.Errorf("rdp.publicHost is required")
	}
	if cfg.Security.DefaultPolicy != "allow" && cfg.Security.DefaultPolicy != "deny" {
		return nil, fmt.Errorf("security.defaultPolicy must be allow or deny")
	}
	if (cfg.Server.QUIC.CertFile == "") != (cfg.Server.QUIC.KeyFile == "") {
		return nil, fmt.Errorf("server.quic.certFile and keyFile must be configured together")
	}
	if cfg.Server.QUIC.CertFile == "" && !cfg.Server.QUIC.AllowEphemeralCertificate {
		return nil, fmt.Errorf("TLS certificate is required; set server.quic.allowEphemeralCertificate only for explicit development use")
	}
	if !cfg.Server.TLS.Disabled && strings.TrimSpace(cfg.Server.TLS.Listen) == "" {
		return nil, fmt.Errorf("server.tls.listen is required when TLS fallback is enabled")
	}
	return cfg, nil
}

func findOrAddMappingChild(mapNode *yaml.Node, key string) *yaml.Node {
	for i := 0; i < len(mapNode.Content); i += 2 {
		if mapNode.Content[i].Value == key {
			return mapNode.Content[i+1]
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"}
	valNode := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	mapNode.Content = append(mapNode.Content, keyNode, valNode)
	return valNode
}

func setMappingScalarChild(mapNode *yaml.Node, key string, tag string, val string) {
	for i := 0; i < len(mapNode.Content); i += 2 {
		if mapNode.Content[i].Value == key {
			mapNode.Content[i+1].Kind = yaml.ScalarNode
			mapNode.Content[i+1].Tag = tag
			mapNode.Content[i+1].Value = val
			return
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"}
	valNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: val}
	mapNode.Content = append(mapNode.Content, keyNode, valNode)
}

func setMappingChild(mapNode *yaml.Node, key string, child *yaml.Node) {
	for i := 0; i < len(mapNode.Content); i += 2 {
		if mapNode.Content[i].Value == key {
			mapNode.Content[i+1] = child
			return
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"}
	mapNode.Content = append(mapNode.Content, keyNode, child)
}

func writeYamlNodeToFile(configPath string, root *yaml.Node) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return fmt.Errorf("encode yaml failed: %w", err)
	}
	_ = enc.Close()

	if err := os.WriteFile(configPath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("write config file failed: %w", err)
	}
	return nil
}

// SyncInvitationsToConfigFile updates the security.enrollmentInvitations section of relay.yaml
// while preserving comments, formatting, and other sections.
func SyncInvitationsToConfigFile(configPath string, invitations []EnrollmentInvitation) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config file failed: %w", err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("unmarshal yaml node failed: %w", err)
	}

	var newInvNode *yaml.Node
	if len(invitations) == 0 {
		newInvNode = &yaml.Node{
			Kind:  yaml.SequenceNode,
			Tag:   "!!seq",
			Style: yaml.FlowStyle,
		}
	} else {
		invData, err := yaml.Marshal(invitations)
		if err != nil {
			return fmt.Errorf("marshal invitations failed: %w", err)
		}
		var invDoc yaml.Node
		if err := yaml.Unmarshal(invData, &invDoc); err != nil {
			return fmt.Errorf("unmarshal new invitations node failed: %w", err)
		}
		if len(invDoc.Content) > 0 {
			newInvNode = invDoc.Content[0]
		} else {
			newInvNode = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		}
	}

	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return fmt.Errorf("invalid yaml root")
	}
	topMap := root.Content[0]
	if topMap.Kind != yaml.MappingNode {
		return fmt.Errorf("root content is not a mapping node")
	}

	secMap := findOrAddMappingChild(topMap, "security")
	setMappingChild(secMap, "enrollmentInvitations", newInvNode)

	return writeYamlNodeToFile(configPath, &root)
}

// SyncAccessPolicyToConfigFile updates security.allowAllControllerTargets and security.controllerAccess
// in relay.yaml while preserving comments, formatting, and other sections.
func SyncAccessPolicyToConfigFile(configPath string, allowAll bool, accessMap map[string][]string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config file failed: %w", err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("unmarshal yaml node failed: %w", err)
	}

	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return fmt.Errorf("invalid yaml root")
	}
	topMap := root.Content[0]
	if topMap.Kind != yaml.MappingNode {
		return fmt.Errorf("root content is not a mapping node")
	}

	secMap := findOrAddMappingChild(topMap, "security")

	// 1. allowAllControllerTargets
	allowAllVal := "false"
	if allowAll {
		allowAllVal = "true"
	}
	setMappingScalarChild(secMap, "allowAllControllerTargets", "!!bool", allowAllVal)

	// 2. controllerAccess
	var newAccessNode *yaml.Node
	if len(accessMap) == 0 {
		newAccessNode = &yaml.Node{
			Kind:  yaml.MappingNode,
			Tag:   "!!map",
			Style: yaml.FlowStyle,
		}
	} else {
		mapBytes, err := yaml.Marshal(accessMap)
		if err != nil {
			return fmt.Errorf("marshal accessMap failed: %w", err)
		}
		var docNode yaml.Node
		if err := yaml.Unmarshal(mapBytes, &docNode); err != nil {
			return fmt.Errorf("unmarshal accessMap node failed: %w", err)
		}
		if len(docNode.Content) > 0 {
			newAccessNode = docNode.Content[0]
		} else {
			newAccessNode = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		}
	}
	setMappingChild(secMap, "controllerAccess", newAccessNode)

	return writeYamlNodeToFile(configPath, &root)
}

// SyncServerSettingsToConfigFile updates general server settings in relay.yaml
// while preserving comments, formatting, and other sections.
func SyncServerSettingsToConfigFile(configPath string, settings ServerSettingsUpdate) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config file failed: %w", err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("unmarshal yaml node failed: %w", err)
	}

	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return fmt.Errorf("invalid yaml root")
	}
	topMap := root.Content[0]
	if topMap.Kind != yaml.MappingNode {
		return fmt.Errorf("root content is not a mapping node")
	}

	if settings.PublicHost != nil {
		rdpMap := findOrAddMappingChild(topMap, "rdp")
		setMappingScalarChild(rdpMap, "publicHost", "!!str", *settings.PublicHost)
	}
	if settings.PortRangeStart != nil || settings.PortRangeEnd != nil {
		rdpMap := findOrAddMappingChild(topMap, "rdp")
		prMap := findOrAddMappingChild(rdpMap, "portRange")
		if settings.PortRangeStart != nil {
			setMappingScalarChild(prMap, "start", "!!int", strconv.Itoa(*settings.PortRangeStart))
		}
		if settings.PortRangeEnd != nil {
			setMappingScalarChild(prMap, "end", "!!int", strconv.Itoa(*settings.PortRangeEnd))
		}
	}
	if settings.DefaultPolicy != nil {
		secMap := findOrAddMappingChild(topMap, "security")
		setMappingScalarChild(secMap, "defaultPolicy", "!!str", *settings.DefaultPolicy)
	}
	if settings.WebToken != nil {
		webMap := findOrAddMappingChild(topMap, "web")
		setMappingScalarChild(webMap, "token", "!!str", *settings.WebToken)
	}
	if settings.QuicListen != nil || settings.TlsListen != nil || settings.TlsDisabled != nil || settings.RendezvousListen != nil {
		serverMap := findOrAddMappingChild(topMap, "server")
		if settings.QuicListen != nil {
			quicMap := findOrAddMappingChild(serverMap, "quic")
			setMappingScalarChild(quicMap, "listen", "!!str", *settings.QuicListen)
		}
		if settings.TlsListen != nil || settings.TlsDisabled != nil {
			tlsMap := findOrAddMappingChild(serverMap, "tls")
			if settings.TlsListen != nil {
				setMappingScalarChild(tlsMap, "listen", "!!str", *settings.TlsListen)
			}
			if settings.TlsDisabled != nil {
				setMappingScalarChild(tlsMap, "disabled", "!!bool", strconv.FormatBool(*settings.TlsDisabled))
			}
		}
		if settings.RendezvousListen != nil {
			rdzvMap := findOrAddMappingChild(serverMap, "rendezvous")
			setMappingScalarChild(rdzvMap, "listen", "!!str", *settings.RendezvousListen)
		}
	}

	return writeYamlNodeToFile(configPath, &root)
}

// SyncRawYamlToConfigFile validates raw YAML content against RelayConfig,
// writes it to configPath if valid, and returns the parsed config.
func SyncRawYamlToConfigFile(configPath string, rawYaml string) (*RelayConfig, error) {
	var cfg RelayConfig
	if err := yaml.Unmarshal([]byte(rawYaml), &cfg); err != nil {
		return nil, fmt.Errorf("YAML 格式校验失败: %w", err)
	}

	if strings.TrimSpace(cfg.Server.QUIC.Listen) == "" {
		return nil, fmt.Errorf("配置校验失败: server.quic.listen 不能为空")
	}

	if err := os.WriteFile(configPath, []byte(rawYaml), 0644); err != nil {
		return nil, fmt.Errorf("写入配置文件失败: %w", err)
	}

	return &cfg, nil
}

// SetDefaults applies standard defaults to unset or non-positive AgentConfig fields.
func (cfg *AgentConfig) SetDefaults() {
	if cfg.RDP.Address == "" {
		cfg.RDP.Address = "127.0.0.1:3389"
	}
	if cfg.Server.QUICDialTimeout <= 0 {
		cfg.Server.QUICDialTimeout = 4 * time.Second
	}
	if cfg.Transport.DatagramPayload <= 0 {
		cfg.Transport.DatagramPayload = 1150
	}
	if cfg.UDP.SessionIdleTimeout <= 0 {
		cfg.UDP.SessionIdleTimeout = 60 * time.Second
	}
	if cfg.UDP.ReassemblyTimeout <= 0 {
		cfg.UDP.ReassemblyTimeout = 100 * time.Millisecond
	}
	if cfg.UDP.MaxFragments <= 0 {
		cfg.UDP.MaxFragments = 64
	}
	if cfg.Heartbeat.Interval <= 0 {
		cfg.Heartbeat.Interval = 10 * time.Second
	}
	if cfg.Reconnect.MaxInterval <= 0 {
		cfg.Reconnect.MaxInterval = 30 * time.Second
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.GUI.Theme != "light" {
		cfg.GUI.Theme = "dark"
	}
}

// DefaultAgentConfig returns an AgentConfig populated with standard defaults.
func DefaultAgentConfig() *AgentConfig {
	cfg := &AgentConfig{}
	cfg.SetDefaults()
	cfg.GUI.MinimizeToTray = true
	return cfg
}

// LoadAgentConfig reads and parses a YAML configuration file for Agent
func LoadAgentConfig(path string) (*AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read agent config file failed: %w", err)
	}

	cfg := DefaultAgentConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse agent config yaml failed: %w", err)
	}
	// Re-apply defaults so that explicit zero/empty values in YAML are safely defaulted
	cfg.SetDefaults()
	if strings.TrimSpace(cfg.Server.Address) == "" {
		return nil, fmt.Errorf("server.address is required")
	}
	if cfg.Server.TLSAddress == "" {
		cfg.Server.TLSAddress = cfg.Server.Address
	}
	if strings.TrimSpace(cfg.Device.ID) == "" {
		return nil, fmt.Errorf("device.id is required")
	}
	if len(cfg.Device.Secret) < 32 {
		return nil, fmt.Errorf("device.secret must be at least 32 bytes")
	}
	if token := cfg.Device.EnrollmentToken; token != "" && len(token) < 32 {
		return nil, fmt.Errorf("device.enrollmentToken must be at least 32 bytes when configured")
	}
	return cfg, nil
}

// DefaultAgentConfigPath returns the preferred path for agent.yaml:
// 1. RDPULSE_CONFIG environment variable if set
// 2. ~/.rdpulse/agent.yaml if it exists
// 3. ./configs/agent.yaml if it exists
// 4. ./agent.yaml if it exists
// 5. Default fallback: ~/.rdpulse/agent.yaml
func DefaultAgentConfigPath() string {
	if env := os.Getenv("RDPULSE_CONFIG"); env != "" {
		return env
	}

	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		userConfig := filepath.Join(home, ".rdpulse", "agent.yaml")
		if _, err := os.Stat(userConfig); err == nil {
			return userConfig
		}
	}

	if _, err := os.Stat("configs/agent.yaml"); err == nil {
		return "configs/agent.yaml"
	}
	if _, err := os.Stat("agent.yaml"); err == nil {
		return "agent.yaml"
	}

	if home != "" {
		return filepath.Join(home, ".rdpulse", "agent.yaml")
	}
	return "configs/agent.yaml"
}
