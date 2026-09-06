package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// Control message types
const (
	// V1 Core Messages
	MsgTypeHello        = "HELLO"
	MsgTypeAuth         = "AUTH"
	MsgTypeAuthOK       = "AUTH_OK"
	MsgTypeRegister     = "REGISTER"
	MsgTypeRegisterAck  = "REGISTER_ACK"
	MsgTypePing         = "PING"
	MsgTypePong         = "PONG"
	MsgTypePortAssign   = "PORT_ASSIGN"
	MsgTypePortRevoke   = "PORT_REVOKE"
	MsgTypeAgentStatus  = "AGENT_STATUS"
	MsgTypeConfigUpdate = "CONFIG_UPDATE"
	MsgTypeUDPClose     = "UDP_CLOSE"
	MsgTypeError        = "ERROR"
	MsgTypeDrain        = "DRAIN"

	// V2 P2P & Rendezvous & Signaling Messages
	MsgTypeConnectRequest    = "CONNECT_REQUEST"
	MsgTypeConnectNotify     = "CONNECT_NOTIFY"
	MsgTypeConnectResponse   = "CONNECT_RESPONSE"
	MsgTypeCandidateExchange = "CANDIDATE_EXCHANGE"
	MsgTypePunchStart        = "PUNCH_START"
	MsgTypePunchResult       = "PUNCH_RESULT"
	MsgTypeRelayAllocate     = "RELAY_ALLOCATE"
	MsgTypePathChange        = "PATH_CHANGE"
	MsgTypeSessionClose      = "SESSION_CLOSE"
)

const (
	MaxControlMessageSize = 1024 * 1024 // 1 MB safety limit for control messages
)

// CandidateInfo represents an endpoint candidate (LAN or Public Server Reflexive)
type CandidateInfo struct {
	Protocol string `json:"protocol"` // "udp" or "tcp"
	Type     string `json:"type"`     // "lan" or "reflexive"
	Address  string `json:"address"`  // "ip:port"
	Priority uint32 `json:"priority"`
}

// FormatCandidates is a compact diagnostic line for client logs.
func FormatCandidates(cands []CandidateInfo) string {
	if len(cands) == 0 {
		return "无"
	}
	parts := make([]string, 0, len(cands))
	for _, c := range cands {
		kind := c.Type
		if kind == "" {
			kind = "?"
		}
		proto := c.Protocol
		if proto == "" {
			proto = "?"
		}
		parts = append(parts, kind+"/"+proto+" "+c.Address)
	}
	return strings.Join(parts, ", ")
}

// ControlMessage is the general envelope for control stream communications
type ControlMessage struct {
	Type               string `json:"type"`
	DeviceID           string `json:"device_id,omitempty"`
	TargetDeviceID     string `json:"target_device_id,omitempty"`
	Secret             string `json:"secret,omitempty"`
	SecretHash         string `json:"secret_hash,omitempty"`
	EnrollmentToken    string `json:"enrollment_token,omitempty"`
	Hostname           string `json:"hostname,omitempty"`
	AgentVersion       string `json:"agent_version,omitempty"`
	MinProtocolVersion uint8  `json:"min_protocol_version,omitempty"`
	MaxProtocolVersion uint8  `json:"max_protocol_version,omitempty"`
	PublicHost         string `json:"public_host,omitempty"`
	PublicPort         uint16 `json:"public_port,omitempty"`
	TargetPort         uint16 `json:"target_port,omitempty"`
	SessionID          uint32 `json:"session_id,omitempty"`
	SessionToken       string `json:"session_token,omitempty"` // For HMAC punch authentication
	Timestamp          int64  `json:"timestamp,omitempty"`
	RDPOnline          bool   `json:"rdp_online,omitempty"`
	Code               int    `json:"code,omitempty"`
	Message            string `json:"message,omitempty"`

	// V2 Signaling additions
	Candidates []CandidateInfo `json:"candidates,omitempty"`
	PathType   string          `json:"path_type,omitempty"`  // "lan", "p2p_udp", "p2p_tcp", "quic_relay", "tls_relay"
	PunchRole  string          `json:"punch_role,omitempty"` // "controller" or "controlled"
}

// ControlWriter serializes framed control messages written to a shared stream.
// A control frame consists of a length prefix followed by JSON; allowing two
// goroutines to write those parts independently corrupts the stream framing.
type ControlWriter struct {
	w  io.Writer
	mu sync.Mutex
}

func NewControlWriter(w io.Writer) *ControlWriter {
	return &ControlWriter{w: w}
}

func (w *ControlWriter) WriteMessage(msg *ControlMessage) error {
	if w == nil || w.w == nil {
		return errors.New("control writer is nil")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return WriteControlMessage(w.w, msg)
}

// ReadControlMessage reads a length-prefixed (4 bytes BigEndian) JSON message from r
func ReadControlMessage(r io.Reader) (*ControlMessage, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	msgLen := binary.BigEndian.Uint32(lenBuf[:])
	if msgLen > MaxControlMessageSize {
		return nil, fmt.Errorf("control message size %d exceeds limit %d", msgLen, MaxControlMessageSize)
	}

	buf := make([]byte, msgLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}

	var msg ControlMessage
	if err := json.Unmarshal(buf, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal control message failed: %w", err)
	}
	return &msg, nil
}

// WriteControlMessage writes a length-prefixed (4 bytes BigEndian) JSON message to w
func WriteControlMessage(w io.Writer, msg *ControlMessage) error {
	if msg == nil {
		return errors.New("cannot write nil control message")
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal control message failed: %w", err)
	}

	if len(data) > MaxControlMessageSize {
		return fmt.Errorf("control message size %d exceeds limit %d", len(data), MaxControlMessageSize)
	}

	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)

	n, err := w.Write(frame)
	if err != nil {
		return err
	}
	if n != len(frame) {
		return io.ErrShortWrite
	}
	return nil
}
