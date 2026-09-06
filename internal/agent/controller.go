package agent

import (
	"context"
	"fmt"
	"log"
	"net"
	"os/exec"
	"runtime"
	"time"

	"rdpulse/internal/controller"
	"rdpulse/internal/path"
	"rdpulse/internal/protocol"
	"rdpulse/internal/transport"
)

// ConnectTarget initiates a P2P + Relay connection to targetDeviceID as a Controller
func (c *Client) ConnectTarget(ctx context.Context, targetDeviceID, localProxyAddr string, autoLaunchMstsc bool) (*controller.LocalProxy, *path.Manager, error) {
	if localProxyAddr == "" {
		localProxyAddr = "127.0.0.1:13389"
	}

	log.Printf("[Controller] 开始连接目标 %s (本机代理 %s, 禁用P2P=%v)", targetDeviceID, localProxyAddr, c.cfg.Transport.DisableP2P)
	log.Printf("[Controller] 正在连接信令/中继 %s ...", c.cfg.Server.Address)
	tr, transportName, err := c.dialRelayTransport(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("dial relay failed: %w", err)
	}
	log.Printf("[Controller] 已连上中继，传输方式: %s", transportName)

	// 1. Open Control Stream
	controlStream, err := tr.OpenStream(ctx)
	if err != nil {
		_ = tr.Close()
		return nil, nil, fmt.Errorf("open control stream failed: %w", err)
	}
	controlWriter := protocol.NewControlWriter(controlStream)

	// 2. Authenticate, disclosing a one-time invitation only when challenged.
	if err := c.authenticateControl(controlStream, controlWriter); err != nil {
		_ = tr.Close()
		return nil, nil, err
	}

	// 3. Open dedicated UDP socket for P2P punching and discover local candidates
	udpPunchConn, err := net.ListenUDP("udp", nil)
	if err != nil {
		_ = tr.Close()
		return nil, nil, fmt.Errorf("listen udp punch conn failed: %w", err)
	}
	tuneUDPSocketBuffers(udpPunchConn)
	p2pUDPPort := udpPunchConn.LocalAddr().(*net.UDPAddr).Port

	log.Printf("[Controller] 本机打洞 UDP 端口: %d", p2pUDPPort)
	localCandidates := c.discoverP2PCandidates(ctx, udpPunchConn, p2pUDPPort, 0)
	log.Printf("[Controller] 本机候选地址 (%d): %s", len(localCandidates), protocol.FormatCandidates(localCandidates))

	// 4. Send CONNECT_REQUEST
	connReq := &protocol.ControlMessage{
		Type:           protocol.MsgTypeConnectRequest,
		DeviceID:       c.cfg.Device.ID,
		TargetDeviceID: targetDeviceID,
		Candidates:     localCandidates,
	}
	if err := controlWriter.WriteMessage(connReq); err != nil {
		_ = udpPunchConn.Close()
		_ = tr.Close()
		return nil, nil, fmt.Errorf("send connect request failed: %w", err)
	}

	log.Printf("[Controller] 已向目标 %s 发送连接请求，等待对方候选地址...", targetDeviceID)

	// 5. Read CONNECT_RESPONSE
	connResp, err := protocol.ReadControlMessage(controlStream)
	if err != nil {
		_ = udpPunchConn.Close()
		_ = tr.Close()
		return nil, nil, fmt.Errorf("read connect response failed: %w", err)
	}

	if connResp.Type == protocol.MsgTypeError {
		_ = udpPunchConn.Close()
		_ = tr.Close()
		return nil, nil, fmt.Errorf("connect failed: %s", connResp.Message)
	}

	if connResp.Type != protocol.MsgTypeConnectResponse {
		_ = udpPunchConn.Close()
		_ = tr.Close()
		return nil, nil, fmt.Errorf("unexpected response type: %s", connResp.Type)
	}

	log.Printf("[Controller] 收到目标候选 (%d 个), SessionID=%d, 中继入口=%s:%d", len(connResp.Candidates), connResp.SessionID, connResp.PublicHost, connResp.PublicPort)
	log.Printf("[Controller] 目标候选地址: %s", protocol.FormatCandidates(connResp.Candidates))

	// 6. Initialize PathManager and launch Happy-Eyeballs parallel competition
	sessionID := uint64(connResp.SessionID)
	sessionToken := []byte(connResp.SessionToken)
	relayTargetAddr := fmt.Sprintf("%s:%d", connResp.PublicHost, connResp.PublicPort)

	pathMgr := path.NewManager(ctx, connResp.SessionID, sessionToken, tr)
	pathMgr.SetRelayEndpoints(relayTargetAddr, relayTargetAddr)

	log.Printf("[Controller] 启动路径竞速 (Happy-Eyeballs): UDP打洞 2.5s, 300ms 后预热中继")
	path.ParallelRace(ctx, udpPunchConn, connResp.Candidates, sessionID, sessionToken, func() error {
		log.Println("[Controller] 300ms 内直连未确认，已预热中继路径")
		return nil
	}, pathMgr)

	go c.controllerControlLoop(ctx, controlStream, connResp.SessionID, pathMgr)
	if !c.cfg.Transport.DisableP2P {
		go c.refreshControllerCandidates(ctx, controlWriter, targetDeviceID, connResp.SessionID, p2pUDPPort, localCandidates)
	}

	// 7. Start local RDP proxy on 127.0.0.1:13389
	proxy, err := controller.StartLocalProxy(ctx, localProxyAddr, relayTargetAddr, pathMgr, tr)
	if err != nil {
		_ = pathMgr.Close()
		_ = tr.Close()
		return nil, nil, fmt.Errorf("start local proxy failed: %w", err)
	}

	// 8. If on Windows and autoLaunchMstsc requested, launch mstsc.exe
	if autoLaunchMstsc && runtime.GOOS == "windows" {
		go func() {
			time.Sleep(500 * time.Millisecond)
			log.Printf("[Controller] 正在启动 mstsc.exe /v:%s ...", localProxyAddr)
			cmd := exec.Command("mstsc.exe", fmt.Sprintf("/v:%s", localProxyAddr))
			_ = cmd.Start()
		}()
	}

	return proxy, pathMgr, nil
}

func (c *Client) controllerControlLoop(ctx context.Context, stream transport.Stream, sessionID uint32, pathMgr *path.Manager) {
	for {
		msg, err := protocol.ReadControlMessage(stream)
		if err != nil {
			return
		}
		if msg.SessionID != 0 && msg.SessionID != sessionID {
			continue
		}
		switch msg.Type {
		case protocol.MsgTypeCandidateExchange:
			log.Printf("[Controller] 收到补充候选: %s", protocol.FormatCandidates(msg.Candidates))
			pathMgr.AddCandidates(msg.Candidates)
		case protocol.MsgTypeSessionClose:
			log.Printf("[Controller] 对端关闭会话 SessionID=%d", sessionID)
			_ = pathMgr.Close()
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}
