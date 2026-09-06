package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"rdpulse/internal/acl"
	"rdpulse/internal/config"
	"rdpulse/internal/metrics"
	"rdpulse/internal/relay"
	"rdpulse/internal/rendezvous"
	"rdpulse/internal/storage"
	"rdpulse/internal/transport"
	"rdpulse/internal/transport/quicgo"
	"rdpulse/internal/transport/tlsmux"
	"rdpulse/internal/web"
)

func main() {
	configPath := flag.String("config", "configs/relay.yaml", "path to relay configuration file")
	flag.Parse()

	log.SetOutput(io.MultiWriter(os.Stderr, web.Hub))
	log.Printf("[RDPulse Relay] Starting with config: %s", *configPath)

	cfg, err := config.LoadRelayConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load relay config: %v", err)
	}

	// 1. Initialize Storage
	db, err := storage.Open(cfg.Storage.Path)
	if err != nil {
		log.Fatalf("Failed to open storage at %s: %v", cfg.Storage.Path, err)
	}
	defer db.Close()

	// 2. Initialize Port Manager
	portMgr, err := relay.NewPortManager(cfg.RDP.PortRange.Start, cfg.RDP.PortRange.End, db)
	if err != nil {
		log.Fatalf("Failed to initialize port manager: %v", err)
	}

	// 3. Initialize ACL Manager
	aclMgr, err := acl.NewManager(cfg.Security.DefaultPolicy, cfg.Security.Allow)
	if err != nil {
		log.Fatalf("Failed to initialize ACL manager: %v", err)
	}

	// 4. Initialize Agent Manager
	agentMgr := relay.NewAgentManager(cfg, db, portMgr, aclMgr)
	defer agentMgr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 5. Start Metrics & pprof Server
	metricsSrv := metrics.StartServer(cfg.Metrics.Listen)
	if metricsSrv != nil {
		log.Printf("[RDPulse Relay] Metrics and pprof listening on %s", cfg.Metrics.Listen)
	}

	// 5.1 Start Web Console Server
	if !cfg.Web.Disabled && cfg.Web.Token == "" {
		tokenBytes := make([]byte, 16)
		if _, err := rand.Read(tokenBytes); err != nil {
			log.Fatalf("[RDPulse Relay] Web Console requires a token and failed to generate one: %v", err)
		}
		cfg.Web.Token = hex.EncodeToString(tokenBytes)
		log.Println("================================================================================")
		log.Printf("[RDPulse Web] Web Console 未在配置文件中指定 token，已自动生成访问 Token:")
		log.Printf("  Token: %s", cfg.Web.Token)
		log.Printf("  请打开 http://%s 并在控制台中粘贴 Token（不要把 Token 放进 URL）", cfg.Web.Listen)
		log.Println("  (提示: 若需固定密钥，可在 configs/relay.yaml 的 web.token 中配置)")
		log.Println("================================================================================")
	}
	webSrv := web.StartServer(ctx, cfg, *configPath, db, agentMgr)

	// 5.2 Start Real-Time Configuration Watcher for Enrollment Invitations & Access Policy
	startConfigWatcher(ctx, *configPath, db, cfg, agentMgr)

	// 6. Setup TLS configuration
	var tlsConf *tls.Config
	if cfg.Server.QUIC.CertFile != "" && cfg.Server.QUIC.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.Server.QUIC.CertFile, cfg.Server.QUIC.KeyFile)
		if err != nil {
			log.Fatalf("Failed to load TLS cert/key: %v", err)
		}
		tlsConf = &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"rdpulse-quic"},
		}
	} else {
		log.Println("[RDPulse Relay] Explicit development mode: generating temporary self-signed TLS 1.3 cert")
		tlsConf, err = quicgo.GenerateSelfSignedCert()
		if err != nil {
			log.Fatalf("Failed to generate self-signed cert: %v", err)
		}
	}

	// 7. Start QUIC Listener
	quicListener, err := quicgo.ListenAddr(cfg.Server.QUIC.Listen, tlsConf, quicgo.DefaultQUICConfig())
	if err != nil {
		log.Fatalf("Failed to listen QUIC on %s: %v", cfg.Server.QUIC.Listen, err)
	}
	defer quicListener.Close()

	var tlsListener transport.Listener
	if !cfg.Server.TLS.Disabled {
		tlsListener, err = tlsmux.Listen(cfg.Server.TLS.Listen, tlsConf)
		if err != nil {
			log.Fatalf("Failed to listen TLS fallback on %s: %v", cfg.Server.TLS.Listen, err)
		}
		defer tlsListener.Close()
		log.Printf("[RDPulse Relay] TLS/TCP fallback listening on %s", cfg.Server.TLS.Listen)
	}

	// 8. Start Rendezvous UDP Server for NAT mapping discovery
	rdzvListen := cfg.Server.Rendezvous.Listen
	if rdzvListen == "" {
		rdzvListen = ":21116"
	}
	rdzvServer, err := rendezvous.StartServer(ctx, rdzvListen)
	if err != nil {
		log.Printf("[RDPulse Relay] Warning: Failed to start Rendezvous server on %s: %v", rdzvListen, err)
	} else {
		defer rdzvServer.Close()
		log.Printf("[RDPulse Relay] Rendezvous NAT Discovery server listening on UDP %s", rdzvListen)
	}

	log.Printf("[RDPulse Relay] QUIC server listening on UDP %s", cfg.Server.QUIC.Listen)
	log.Printf("[RDPulse Relay] Public RDP port range: %d - %d", cfg.RDP.PortRange.Start, cfg.RDP.PortRange.End)

	// Graceful shutdown handler
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("[RDPulse Relay] Shutting down...")
		cancel()
		_ = quicListener.Close()
		if tlsListener != nil {
			_ = tlsListener.Close()
		}
	}()

	acceptErr := make(chan error, 2)
	go serveTransport(ctx, "QUIC", quicListener, agentMgr, acceptErr)
	if tlsListener != nil {
		go serveTransport(ctx, "TLS/TCP", tlsListener, agentMgr, acceptErr)
	}
	select {
	case <-ctx.Done():
		if metricsSrv != nil {
			shutdownCtx, sCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer sCancel()
			_ = metricsSrv.Shutdown(shutdownCtx)
		}
		if webSrv != nil {
			shutdownCtx, sCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer sCancel()
			_ = webSrv.Shutdown(shutdownCtx)
		}
		log.Println("[RDPulse Relay] Server stopped cleanly.")
		return
	case err := <-acceptErr:
		log.Fatalf("Relay listener stopped: %v", err)
	}
}

func startConfigWatcher(ctx context.Context, configPath string, db *storage.DB, activeCfg *config.RelayConfig, agentMgr *relay.AgentManager) {
	stat, err := os.Stat(configPath)
	if err != nil {
		return
	}
	lastMod := stat.ModTime()

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				st, err := os.Stat(configPath)
				if err != nil {
					continue
				}
				if st.ModTime().After(lastMod) {
					lastMod = st.ModTime()
					reloaded, err := config.LoadRelayConfig(configPath)
					if err != nil {
						// Don't crash on intermediate saves or syntax errors
						continue
					}
					synced := 0
					for _, inv := range reloaded.Security.EnrollmentInvitations {
						if err := db.UpsertEnrollmentInvitation(inv.DeviceID, inv.Token, inv.ExpiresAt); err == nil {
							synced++
						}
					}
					activeCfg.Security.EnrollmentInvitations = reloaded.Security.EnrollmentInvitations
					activeCfg.Security.AllowAllControllerTargets = reloaded.Security.AllowAllControllerTargets
					activeCfg.Security.ControllerAccess = reloaded.Security.ControllerAccess
					if agentMgr != nil {
						agentMgr.UpdateAccessPolicy(reloaded.Security.AllowAllControllerTargets, reloaded.Security.ControllerAccess)
					}
					log.Printf("[RDPulse Relay] 配置文件 %s 发生变更，已实时热重载并同步 %d 条邀请凭证与访问授权策略（立即生效）", configPath, synced)
				}
			}
		}
	}()
}

func serveTransport(ctx context.Context, name string, listener transport.Listener, manager *relay.AgentManager, errors chan<- error) {
	for {
		tr, err := listener.Accept(ctx)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				errors <- fmt.Errorf("%s accept: %w", name, err)
				return
			}
		}
		go manager.HandleIncomingConnection(ctx, tr)
	}
}
