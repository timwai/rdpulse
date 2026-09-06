package punch

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"

	"rdpulse/internal/protocol"
)

const (
	tcpChallengeSize = 16
	tcpRoleClient    = 1
	tcpRoleServer    = 2
	tcpRoleProbe     = 3
	tcpRoleProbeAck  = 4
)

var (
	ErrTCPPunchTimeout   = errors.New("tcp punch timed out")
	ErrTCPAuthFailed     = errors.New("tcp peer authentication failed")
	ErrTCPUnknownSession = errors.New("tcp peer session is not active")
	ErrTCPProbe          = errors.New("tcp probe handshake completed")
)

// PunchTCP attempts direct TCP connection to peer candidates with simultaneous connect fallback
func PunchTCP(
	ctx context.Context,
	candidates []protocol.CandidateInfo,
	sessionID uint64,
	sessionToken []byte,
	timeout time.Duration,
) (net.Conn, error) {
	return dialTCPAuth(ctx, candidates, sessionID, sessionToken, timeout, tcpRoleClient)
}

// ProbeTCP confirms that a direct TCP handshake works without opening an RDP data session.
func ProbeTCP(
	ctx context.Context,
	candidates []protocol.CandidateInfo,
	sessionID uint64,
	sessionToken []byte,
	timeout time.Duration,
) error {
	conn, err := dialTCPAuth(ctx, candidates, sessionID, sessionToken, timeout, tcpRoleProbe)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func dialTCPAuth(
	ctx context.Context,
	candidates []protocol.CandidateInfo,
	sessionID uint64,
	sessionToken []byte,
	timeout time.Duration,
	role byte,
) (net.Conn, error) {
	if len(sessionToken) == 0 {
		return nil, ErrTCPAuthFailed
	}
	if timeout <= 0 {
		timeout = 2500 * time.Millisecond
	}

	punchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var tcpCandidates []protocol.CandidateInfo
	for _, cand := range candidates {
		if cand.Protocol == "tcp" {
			tcpCandidates = append(tcpCandidates, cand)
		}
	}

	sort.Slice(tcpCandidates, func(i, j int) bool {
		return tcpCandidates[i].Priority > tcpCandidates[j].Priority
	})

	if len(tcpCandidates) == 0 {
		return nil, errors.New("no tcp candidates available for punching")
	}

	resultCh := make(chan net.Conn, 1)
	var once sync.Once
	var pendingMu sync.Mutex
	pending := make([]net.Conn, 0, len(tcpCandidates))

	closePending := func(except net.Conn) {
		pendingMu.Lock()
		defer pendingMu.Unlock()
		for _, conn := range pending {
			if conn != nil && conn != except {
				_ = conn.Close()
			}
		}
	}

	for _, cand := range tcpCandidates {
		targetAddr := cand.Address
		go func() {
			dialer := &net.Dialer{
				Timeout:   timeout,
				KeepAlive: 15 * time.Second,
			}

			conn, err := dialer.DialContext(punchCtx, "tcp", targetAddr)
			if err != nil {
				return
			}
			pendingMu.Lock()
			pending = append(pending, conn)
			pendingMu.Unlock()

			if err := authenticateTCPPeer(punchCtx, conn, sessionID, sessionToken, role); err != nil {
				_ = conn.Close()
				return
			}
			if punchCtx.Err() != nil {
				_ = conn.Close()
				return
			}

			won := false
			once.Do(func() {
				won = true
				resultCh <- conn
			})
			if !won {
				_ = conn.Close()
			}
		}()
	}

	select {
	case conn := <-resultCh:
		closePending(conn)
		return conn, nil
	case <-punchCtx.Done():
		closePending(nil)
		return nil, ErrTCPPunchTimeout
	}
}

func authenticateTCPPeer(ctx context.Context, conn net.Conn, sessionID uint64, sessionToken []byte, role byte) error {
	if err := setTCPAuthDeadline(ctx, conn); err != nil {
		return err
	}
	defer conn.SetDeadline(time.Time{})

	if len(sessionToken) == 0 {
		return ErrTCPAuthFailed
	}

	var challenge [tcpChallengeSize]byte
	if _, err := io.ReadFull(conn, challenge[:]); err != nil {
		return fmt.Errorf("read tcp auth challenge failed: %w", err)
	}

	authBuf := tcpAuthTag(sessionID, sessionToken, role, challenge[:])
	if _, err := conn.Write(authBuf[:]); err != nil {
		return fmt.Errorf("write tcp auth failed: %w", err)
	}

	var peerAuth [24]byte
	if _, err := io.ReadFull(conn, peerAuth[:]); err != nil {
		return fmt.Errorf("read tcp peer auth failed: %w", err)
	}

	peerSessionID := binary.BigEndian.Uint64(peerAuth[0:8])
	if peerSessionID != sessionID {
		return ErrTCPAuthFailed
	}

	expectedRole := byte(tcpRoleServer)
	if role == tcpRoleProbe {
		expectedRole = tcpRoleProbeAck
	}
	expected := tcpAuthTag(sessionID, sessionToken, expectedRole, challenge[:])
	if !hmac.Equal(peerAuth[:], expected[:]) {
		return ErrTCPAuthFailed
	}

	return nil
}

// AcceptTCPPeer authenticates the server side of a direct TCP connection. The
// token lookup keeps session secrets out of the listener and permits multiple
// concurrent RDP connections for one signaling session.
func AcceptTCPPeer(conn net.Conn, lookup func(sessionID uint64) ([]byte, bool)) (uint64, error) {
	if conn == nil || lookup == nil {
		return 0, ErrTCPAuthFailed
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	defer conn.SetDeadline(time.Time{})

	var challenge [tcpChallengeSize]byte
	if _, err := rand.Read(challenge[:]); err != nil {
		return 0, fmt.Errorf("generate tcp auth challenge failed: %w", err)
	}
	if _, err := conn.Write(challenge[:]); err != nil {
		return 0, fmt.Errorf("write tcp auth challenge failed: %w", err)
	}

	var peerAuth [24]byte
	if _, err := io.ReadFull(conn, peerAuth[:]); err != nil {
		return 0, fmt.Errorf("read tcp peer auth failed: %w", err)
	}
	sessionID := binary.BigEndian.Uint64(peerAuth[0:8])
	token, ok := lookup(sessionID)
	if !ok || len(token) == 0 {
		return 0, ErrTCPUnknownSession
	}

	dataTag := tcpAuthTag(sessionID, token, tcpRoleClient, challenge[:])
	probeTag := tcpAuthTag(sessionID, token, tcpRoleProbe, challenge[:])
	switch {
	case hmac.Equal(peerAuth[:], dataTag[:]):
		response := tcpAuthTag(sessionID, token, tcpRoleServer, challenge[:])
		if _, err := conn.Write(response[:]); err != nil {
			return 0, fmt.Errorf("write tcp auth response failed: %w", err)
		}
		return sessionID, nil
	case hmac.Equal(peerAuth[:], probeTag[:]):
		response := tcpAuthTag(sessionID, token, tcpRoleProbeAck, challenge[:])
		if _, err := conn.Write(response[:]); err != nil {
			return 0, fmt.Errorf("write tcp probe response failed: %w", err)
		}
		return sessionID, ErrTCPProbe
	default:
		return 0, ErrTCPAuthFailed
	}
}

func setTCPAuthDeadline(ctx context.Context, conn net.Conn) error {
	deadline := time.Now().Add(2 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return conn.SetDeadline(deadline)
}

func tcpAuthTag(sessionID uint64, token []byte, role byte, challenge []byte) [24]byte {
	var tag [24]byte
	binary.BigEndian.PutUint64(tag[0:8], sessionID)
	h := hmac.New(sha256.New, token)
	_, _ = h.Write([]byte{role})
	_, _ = h.Write(tag[0:8])
	_, _ = h.Write(challenge)
	copy(tag[8:24], h.Sum(nil)[:16])
	return tag
}
