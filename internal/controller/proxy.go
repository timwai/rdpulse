package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"rdpulse/internal/path"
	"rdpulse/internal/protocol"
	"rdpulse/internal/tcp"
	"rdpulse/internal/transport"
)

// LocalProxy binds local port 13389 (TCP+UDP) for mstsc to connect to
type LocalProxy struct {
	listenAddr      string
	relayTargetAddr string
	pathMgr         *path.Manager
	relayTr         transport.Transport
	tcpListener     net.Listener
	udpConn         *net.UDPConn
	closed          bool
	mu              sync.Mutex
}

// StartLocalProxy creates and starts the local controller proxy
func StartLocalProxy(ctx context.Context, listenAddr, relayTargetAddr string, pathMgr *path.Manager, relayTr transport.Transport) (*LocalProxy, error) {
	if listenAddr == "" {
		listenAddr = "127.0.0.1:13389"
	}

	tcpL, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen local tcp failed: %w", err)
	}

	udpAddr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		_ = tcpL.Close()
		return nil, fmt.Errorf("resolve local udp failed: %w", err)
	}

	udpC, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		_ = tcpL.Close()
		return nil, fmt.Errorf("listen local udp failed: %w", err)
	}
	_ = udpC.SetReadBuffer(2 * 1024 * 1024)
	_ = udpC.SetWriteBuffer(2 * 1024 * 1024)

	p := &LocalProxy{
		listenAddr:      listenAddr,
		relayTargetAddr: relayTargetAddr,
		pathMgr:         pathMgr,
		relayTr:         relayTr,
		tcpListener:     tcpL,
		udpConn:         udpC,
	}

	go p.serveTCP(ctx)
	go p.serveUDP(ctx)

	log.Printf("[Controller] 本机 RDP 代理已监听 %s (TCP + UDP)，中继目标 %s", listenAddr, relayTargetAddr)
	return p, nil
}

func (p *LocalProxy) Addr() string {
	return p.listenAddr
}

func (p *LocalProxy) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.tcpListener != nil {
		_ = p.tcpListener.Close()
	}
	if p.udpConn != nil {
		_ = p.udpConn.Close()
	}
	if p.relayTr != nil {
		_ = p.relayTr.Close()
	}
	return nil
}

func (p *LocalProxy) serveTCP(ctx context.Context) {
	for {
		clientConn, err := p.tcpListener.Accept()
		if err != nil {
			if p.closed {
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}

		go p.handleTCPClient(ctx, clientConn)
	}
}

func (p *LocalProxy) handleTCPClient(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()

	activePath := p.pathMgr.TCPPath()
	log.Printf("[Controller] 收到本机 mstsc TCP 连接 %s，走 %s", clientConn.RemoteAddr(), activePath)

	switch activePath {
	case path.PathDirectLAN, path.PathDirectTCP:
		p.proxyViaDirectTCP(ctx, clientConn)
	default:
		p.proxyViaRelay(ctx, clientConn)
	}
}

func (p *LocalProxy) proxyViaDirectTCP(ctx context.Context, clientConn net.Conn) {
	dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	directConn, err := p.pathMgr.DialDirectTCP(dialCtx)
	if err != nil {
		log.Printf("[Controller] 直连 TCP 不可用 (%v)，改走中继", err)
		p.proxyViaRelay(ctx, clientConn)
		return
	}
	defer directConn.Close()

	log.Printf("[Controller] 正在通过直连 TCP 转发 mstsc -> %s", directConn.RemoteAddr())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tcp.Bridge(clientConn, directConn)
	}()

	select {
	case <-ctx.Done():
		_ = clientConn.Close()
		_ = directConn.Close()
	case <-done:
	}
}

func (p *LocalProxy) proxyViaRelay(ctx context.Context, clientConn net.Conn) {
	if p.relayTr == nil {
		log.Println("[Controller] 无法转发 TCP：中继传输不可用")
		return
	}

	openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	relayConn, err := p.relayTr.OpenStream(openCtx)
	if err != nil {
		log.Printf("[Controller] 打开中继 TCP 流失败: %v", err)
		return
	}
	defer relayConn.Close()
	log.Printf("[Controller] 正在通过中继转发 mstsc TCP -> %s", p.relayTargetAddr)

	// tcp.Proxy tunes the mstsc-facing socket and copies through pooled buffers.
	proxy := tcp.NewProxy(clientConn, relayConn, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = proxy.Run()
	}()

	select {
	case <-ctx.Done():
		proxy.Close()
	case <-done:
	}
}

func (p *LocalProxy) serveUDP(ctx context.Context) {
	buf := make([]byte, protocol.MaxUDPPacketSize)

	// lastClientAddr is a value type, so tracking mstsc's address costs no
	// allocation and no lock on the per-packet path.
	var lastClientAddr atomic.Pointer[netip.AddrPort]

	// Background goroutine to receive from PathManager and write back to client
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			payload, err := p.pathMgr.ReceiveUDP(ctx)
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
					return
				}
				continue
			}

			clientAddr := lastClientAddr.Load()
			if clientAddr != nil && len(payload) > 0 {
				_, _ = p.udpConn.WriteToUDPAddrPort(payload, *clientAddr)
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, remoteAddr, err := p.udpConn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if p.closed {
				return
			}
			continue
		}

		if current := lastClientAddr.Load(); current == nil || *current != remoteAddr {
			addr := remoteAddr
			lastClientAddr.Store(&addr)
			log.Printf("[Controller] 本机 mstsc UDP 源地址: %s", remoteAddr)
		}

		// Send UDP payload via PathManager
		_ = p.pathMgr.SendUDP(buf[:n])
	}
}
