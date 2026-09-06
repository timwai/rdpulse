package agent

import (
	"fmt"
	"log"

	"rdpulse/internal/protocol"
	"rdpulse/internal/transport"
)

// authenticateControl deliberately omits the enrollment invitation on normal
// reconnects. It is disclosed only after the Relay confirms that this device
// is unknown, limiting the lifetime and exposure of the one-time secret.
func (c *Client) authenticateControl(stream transport.Stream, writer *protocol.ControlWriter) error {
	auth := &protocol.ControlMessage{
		Type:     protocol.MsgTypeAuth,
		DeviceID: c.cfg.Device.ID,
		Secret:   c.cfg.Device.Secret,
	}
	log.Printf("[Auth] 正在以设备 %s 登录中继...", c.cfg.Device.ID)
	if err := writer.WriteMessage(auth); err != nil {
		return fmt.Errorf("send auth failed: %w", err)
	}
	response, err := protocol.ReadControlMessage(stream)
	if err != nil {
		return fmt.Errorf("read auth response failed: %w", err)
	}
	if response.Type == protocol.MsgTypeError && response.Code == 428 {
		log.Printf("[Auth] 中继要求首次入网邀请码 (428)")
		token := c.cfg.Device.EnrollmentToken
		if token == "" {
			// Fallback: try Device.Secret in case user configured the invitation token as the secret
			token = c.cfg.Device.Secret
		}
		if token == "" {
			return fmt.Errorf("auth rejected: %s", response.Message)
		}
		auth.EnrollmentToken = token
		if err := writer.WriteMessage(auth); err != nil {
			return fmt.Errorf("send enrollment auth failed: %w", err)
		}
		response, err = protocol.ReadControlMessage(stream)
		if err != nil {
			return fmt.Errorf("read enrollment response failed: %w", err)
		}
	}
	if response.Type != protocol.MsgTypeAuthOK {
		return fmt.Errorf("auth rejected: code=%d message=%s", response.Code, response.Message)
	}
	log.Printf("[Auth] 设备 %s 认证通过", c.cfg.Device.ID)
	return nil
}
