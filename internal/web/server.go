package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"rdpulse/internal/config"
	"rdpulse/internal/relay"
	"rdpulse/internal/storage"
)

type Server struct {
	httpServer *http.Server
	cfg        *config.RelayConfig
	configPath string
	cfgMu      sync.Mutex
	db         *storage.DB
	agentMgr   *relay.AgentManager
	startTime  time.Time
}

const webAuthCookie = "rdpulse_web_token"

func StartServer(ctx context.Context, cfg *config.RelayConfig, configPath string, db *storage.DB, agentMgr *relay.AgentManager) *Server {
	if cfg.Web.Disabled {
		return nil
	}
	if strings.TrimSpace(cfg.Web.Token) == "" {
		log.Printf("[RDPulse Web] refusing to start: web.token is empty")
		return nil
	}

	listenAddr := cfg.Web.Listen
	if listenAddr == "" {
		listenAddr = "127.0.0.1:8080"
	}

	s := &Server{
		cfg:        cfg,
		configPath: configPath,
		db:         db,
		agentMgr:   agentMgr,
		startTime:  time.Now(),
	}

	mux := http.NewServeMux()

	// Static assets
	mux.Handle("/", StaticHandler())

	// API endpoints
	mux.HandleFunc("/api/overview", s.authWrap(s.handleOverview))
	mux.HandleFunc("/api/devices", s.authWrap(s.handleDevices))
	mux.HandleFunc("/api/devices/", s.authWrap(s.handleDeviceAction))
	mux.HandleFunc("/api/invitations", s.authWrap(s.handleInvitations))
	mux.HandleFunc("/api/invitations/", s.authWrap(s.handleInvitationAction))
	mux.HandleFunc("/api/access", s.authWrap(s.handleAccess))
	mux.HandleFunc("/api/config", s.authWrap(s.handleConfig))
	mux.HandleFunc("/api/logs", s.authWrap(s.handleLogs))

	s.httpServer = &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("[RDPulse Web] Web Console listening on http://%s", listenAddr)
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[RDPulse Web] Web server error: %v", err)
		}
	}()

	return s
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

func (s *Server) authWrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(s.cfg.Web.Token)
		if token == "" {
			http.Error(w, `{"error":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}

		authHeader := r.Header.Get("Authorization")
		reqToken := ""
		usedBearer := false
		if strings.HasPrefix(authHeader, "Bearer ") {
			reqToken = strings.TrimPrefix(authHeader, "Bearer ")
			usedBearer = true
		} else if cookie, err := r.Cookie(webAuthCookie); err == nil {
			reqToken = cookie.Value
		}
		if subtle.ConstantTimeCompare([]byte(reqToken), []byte(token)) != 1 {
			http.Error(w, `{"error":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if usedBearer {
			http.SetCookie(w, &http.Cookie{
				Name:     webAuthCookie,
				Value:    token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
		}
		next(w, r)
	}
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	registered := 0
	if s.db != nil {
		if agents, err := s.db.GetAllAgents(); err == nil {
			registered = len(agents)
		}
	}

	onlineDevices := s.agentMgr.GetOnlineDevices()

	resp := map[string]interface{}{
		"onlineCount":      len(onlineDevices),
		"registeredCount":  registered,
		"activeSessions":   len(onlineDevices),
		"uptimeSeconds":    int(time.Since(s.startTime).Seconds()),
		"quicListen":       s.cfg.Server.QUIC.Listen,
		"rendezvousListen": s.cfg.Server.Rendezvous.Listen,
		"publicHost":       s.cfg.RDP.PublicHost,
		"portRangeStart":   s.cfg.RDP.PortRange.Start,
		"portRangeEnd":     s.cfg.RDP.PortRange.End,
	}
	writeJSON(w, resp)
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	onlineMap := make(map[string]relay.OnlineDeviceInfo)
	for _, dev := range s.agentMgr.GetOnlineDevices() {
		onlineMap[dev.DeviceID] = dev
	}

	type DeviceItem struct {
		DeviceID   string `json:"deviceId"`
		Hostname   string `json:"hostname"`
		PublicPort uint16 `json:"publicPort"`
		Enabled    bool   `json:"enabled"`
		Online     bool   `json:"online"`
		RemoteAddr string `json:"remoteAddr"`
		LastSeen   string `json:"lastSeen"`
		RDPOnline  bool   `json:"rdpOnline"`
	}

	var list []DeviceItem
	if s.db != nil {
		agents, err := s.db.GetAllAgents()
		if err == nil {
			for _, a := range agents {
				item := DeviceItem{
					DeviceID:   a.DeviceID,
					Hostname:   a.Hostname,
					PublicPort: a.PublicPort,
					Enabled:    a.Enabled,
					LastSeen:   a.LastSeen.Format("2006-01-02 15:04:05"),
				}
				if online, ok := onlineMap[a.DeviceID]; ok {
					item.Online = true
					item.RemoteAddr = online.RemoteAddr
					item.RDPOnline = online.RDPOnline
					delete(onlineMap, a.DeviceID)
				}
				list = append(list, item)
			}
		}
	}

	// Any online devices not in DB (edge case)
	for _, online := range onlineMap {
		list = append(list, DeviceItem{
			DeviceID:   online.DeviceID,
			Hostname:   online.Hostname,
			PublicPort: online.PublicPort,
			Enabled:    true,
			Online:     true,
			RemoteAddr: online.RemoteAddr,
			RDPOnline:  online.RDPOnline,
			LastSeen:   time.Now().Format("2006-01-02 15:04:05"),
		})
	}

	writeJSON(w, list)
}

func validDeviceID(deviceID string) bool {
	if deviceID == "" || len(deviceID) > 64 {
		return false
	}
	for i := 0; i < len(deviceID); i++ {
		ch := deviceID[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.' {
			continue
		}
		return false
	}
	return true
}

func (s *Server) handleDeviceAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/devices/")
	parts := strings.Split(path, "/")
	deviceID := parts[0]

	if deviceID == "" {
		http.Error(w, `{"error":"device ID required"}`, http.StatusBadRequest)
		return
	}

	if len(parts) == 1 {
		if r.Method == http.MethodDelete {
			if s.db != nil {
				if err := s.db.DeleteAgent(deviceID); err != nil {
					http.Error(w, fmt.Sprintf(`{"error":"delete agent failed: %v"}`, err), http.StatusInternalServerError)
					return
				}
			}
			s.agentMgr.DisconnectDevice(deviceID)
			s.agentMgr.ReleaseDevicePort(deviceID)
			writeJSON(w, map[string]string{"status": "deleted"})
			return
		}
		http.Error(w, `{"error":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	if len(parts) > 1 {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"Method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		action := parts[1]
		switch action {
		case "disconnect":
			s.agentMgr.DisconnectDevice(deviceID)
			writeJSON(w, map[string]string{"status": "disconnected"})
			return
		case "toggle":
			var body struct {
				Enabled bool `json:"enabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
				return
			}
			if s.db != nil {
				if err := s.db.SetAgentEnabled(deviceID, body.Enabled); err != nil {
					http.Error(w, fmt.Sprintf(`{"error":"update agent failed: %v"}`, err), http.StatusInternalServerError)
					return
				}
			}
			if !body.Enabled {
				s.agentMgr.DisconnectDevice(deviceID)
			}
			writeJSON(w, map[string]string{"status": "updated"})
			return
		}
	}

	http.Error(w, `{"error":"Not found"}`, http.StatusNotFound)
}

func (s *Server) handleInvitations(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if s.db == nil {
			writeJSON(w, []interface{}{})
			return
		}
		invs, err := s.db.ListEnrollmentInvitations()
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"list invitations failed: %v"}`, err), http.StatusInternalServerError)
			return
		}
		if invs == nil {
			invs = []*storage.EnrollmentInvitationRecord{}
		}
		writeJSON(w, invs)
		return
	}

	if r.Method == http.MethodPost {
		var req struct {
			DeviceID      string `json:"deviceId"`
			Token         string `json:"token"`
			ExpiresInDays int    `json:"expiresInDays"`
			ExpiresAt     string `json:"expiresAt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		req.DeviceID = strings.TrimSpace(req.DeviceID)
		if !validDeviceID(req.DeviceID) {
			http.Error(w, `{"error":"deviceId is required and must contain only alphanumeric, dash, dot, or underscore (max 64 chars)"}`, http.StatusBadRequest)
			return
		}
		expiresAt, err := resolveInvitationExpiry(req.ExpiresAt, req.ExpiresInDays)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadRequest)
			return
		}

		token := strings.TrimSpace(req.Token)
		if len(token) < 32 {
			randBytes := make([]byte, 24)
			if _, err := rand.Read(randBytes); err != nil {
				http.Error(w, `{"error":"failed to generate random token"}`, http.StatusInternalServerError)
				return
			}
			token = hex.EncodeToString(randBytes)
		}

		if s.db != nil {
			if err := s.db.UpsertEnrollmentInvitation(req.DeviceID, token, expiresAt); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":"save invitation failed: %v"}`, err), http.StatusInternalServerError)
				return
			}
		}

		// Update in-memory cfg and sync to config file
		s.cfgMu.Lock()
		found := false
		for i, inv := range s.cfg.Security.EnrollmentInvitations {
			if inv.DeviceID == req.DeviceID {
				s.cfg.Security.EnrollmentInvitations[i].Token = token
				s.cfg.Security.EnrollmentInvitations[i].ExpiresAt = expiresAt
				found = true
				break
			}
		}
		if !found {
			s.cfg.Security.EnrollmentInvitations = append(s.cfg.Security.EnrollmentInvitations, config.EnrollmentInvitation{
				DeviceID:  req.DeviceID,
				Token:     token,
				ExpiresAt: expiresAt,
			})
		}
		invsCopy := make([]config.EnrollmentInvitation, len(s.cfg.Security.EnrollmentInvitations))
		copy(invsCopy, s.cfg.Security.EnrollmentInvitations)
		s.cfgMu.Unlock()

		if s.configPath != "" {
			if err := config.SyncInvitationsToConfigFile(s.configPath, invsCopy); err != nil {
				log.Printf("[RDPulse Web] Warning: failed to sync invitation to config file %s: %v", s.configPath, err)
			} else {
				log.Printf("[RDPulse Web] Synced enrollment invitation for %s to %s", req.DeviceID, s.configPath)
			}
		}

		clientYaml := fmt.Sprintf(`server:
  address: "%s:%s"
  rendezvousAddress: "%s:%s"

device:
  id: "%s"
  secret: "%s"
  enrollmentToken: "%s"
`, s.cfg.RDP.PublicHost, extractPort(s.cfg.Server.QUIC.Listen, "443"),
			s.cfg.RDP.PublicHost, extractPort(s.cfg.Server.Rendezvous.Listen, "21116"),
			req.DeviceID, token, token)

		writeJSON(w, map[string]interface{}{
			"deviceId":   req.DeviceID,
			"token":      token,
			"expiresAt":  expiresAt.Format("2006-01-02 15:04:05"),
			"yamlConfig": clientYaml,
		})
		return
	}

	http.Error(w, `{"error":"Method not allowed"}`, http.StatusMethodNotAllowed)
}

func (s *Server) handleInvitationAction(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.TrimPrefix(r.URL.Path, "/api/invitations/")
	if deviceID == "" {
		http.Error(w, `{"error":"device ID required"}`, http.StatusBadRequest)
		return
	}

	if r.Method == http.MethodDelete {
		if s.db != nil {
			if err := s.db.DeleteEnrollmentInvitation(deviceID); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":"delete invitation failed: %v"}`, err), http.StatusInternalServerError)
				return
			}
		}

		// Update in-memory cfg and sync to config file
		s.cfgMu.Lock()
		newInvs := make([]config.EnrollmentInvitation, 0, len(s.cfg.Security.EnrollmentInvitations))
		for _, inv := range s.cfg.Security.EnrollmentInvitations {
			if inv.DeviceID != deviceID {
				newInvs = append(newInvs, inv)
			}
		}
		s.cfg.Security.EnrollmentInvitations = newInvs
		invsCopy := make([]config.EnrollmentInvitation, len(newInvs))
		copy(invsCopy, newInvs)
		s.cfgMu.Unlock()

		if s.configPath != "" {
			if err := config.SyncInvitationsToConfigFile(s.configPath, invsCopy); err != nil {
				log.Printf("[RDPulse Web] Warning: failed to sync invitation deletion to config file %s: %v", s.configPath, err)
			} else {
				log.Printf("[RDPulse Web] Synced invitation deletion for %s to %s", deviceID, s.configPath)
			}
		}

		writeJSON(w, map[string]string{"status": "deleted"})
		return
	}

	if r.Method == http.MethodPatch {
		var req struct {
			ExpiresAt     string `json:"expiresAt"`
			ExpiresInDays int    `json:"expiresInDays"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.ExpiresAt) == "" && req.ExpiresInDays <= 0 {
			http.Error(w, `{"error":"expiresAt or expiresInDays is required"}`, http.StatusBadRequest)
			return
		}
		expiresAt, err := resolveInvitationExpiry(req.ExpiresAt, req.ExpiresInDays)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadRequest)
			return
		}
		if s.db != nil {
			if err := s.db.UpdateEnrollmentInvitationExpiry(deviceID, expiresAt); err != nil {
				if errors.Is(err, storage.ErrInvitationNotFound) {
					http.Error(w, `{"error":"invitation not found"}`, http.StatusNotFound)
					return
				}
				http.Error(w, fmt.Sprintf(`{"error":"update invitation failed: %v"}`, err), http.StatusInternalServerError)
				return
			}
		}
		s.cfgMu.Lock()
		for i, inv := range s.cfg.Security.EnrollmentInvitations {
			if inv.DeviceID == deviceID {
				s.cfg.Security.EnrollmentInvitations[i].ExpiresAt = expiresAt
				break
			}
		}
		invsCopy := make([]config.EnrollmentInvitation, len(s.cfg.Security.EnrollmentInvitations))
		copy(invsCopy, s.cfg.Security.EnrollmentInvitations)
		s.cfgMu.Unlock()
		if s.configPath != "" {
			if err := config.SyncInvitationsToConfigFile(s.configPath, invsCopy); err != nil {
				log.Printf("[RDPulse Web] Warning: failed to sync invitation expiry to config file %s: %v", s.configPath, err)
			}
		}
		writeJSON(w, map[string]string{
			"status":    "updated",
			"expiresAt": expiresAt.Format("2006-01-02 15:04:05"),
		})
		return
	}

	http.Error(w, `{"error":"Method not allowed"}`, http.StatusMethodNotAllowed)
}

func (s *Server) handleAccess(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		allowAll, accessMap := s.agentMgr.GetAccessPolicy()
		writeJSON(w, map[string]interface{}{
			"allowAll":  allowAll,
			"accessMap": accessMap,
		})
		return
	}

	if r.Method == http.MethodPost {
		var req struct {
			AllowAll  bool                `json:"allowAll"`
			AccessMap map[string][]string `json:"accessMap"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		if req.AccessMap == nil {
			req.AccessMap = make(map[string][]string)
		}

		s.agentMgr.UpdateAccessPolicy(req.AllowAll, req.AccessMap)

		s.cfgMu.Lock()
		s.cfg.Security.AllowAllControllerTargets = req.AllowAll
		s.cfg.Security.ControllerAccess = req.AccessMap
		s.cfgMu.Unlock()

		if s.configPath != "" {
			if err := config.SyncAccessPolicyToConfigFile(s.configPath, req.AllowAll, req.AccessMap); err != nil {
				log.Printf("[RDPulse Web] Warning: failed to sync access policy to config file %s: %v", s.configPath, err)
			} else {
				log.Printf("[RDPulse Web] Synced access policy to %s (allowAll=%v, targets=%d)", s.configPath, req.AllowAll, len(req.AccessMap))
			}
		}

		writeJSON(w, map[string]string{"status": "updated"})
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.cfgMu.Lock()
		resp := map[string]interface{}{
			"quicListen":       s.cfg.Server.QUIC.Listen,
			"tlsListen":        s.cfg.Server.TLS.Listen,
			"tlsDisabled":      s.cfg.Server.TLS.Disabled,
			"rendezvousListen": s.cfg.Server.Rendezvous.Listen,
			"publicHost":       s.cfg.RDP.PublicHost,
			"portRangeStart":   s.cfg.RDP.PortRange.Start,
			"portRangeEnd":     s.cfg.RDP.PortRange.End,
			"defaultPolicy":    s.cfg.Security.DefaultPolicy,
			"webListen":        s.cfg.Web.Listen,
			"webToken":         s.cfg.Web.Token,
		}
		s.cfgMu.Unlock()

		if s.configPath != "" {
			if yamlBytes, err := os.ReadFile(s.configPath); err == nil {
				resp["rawYaml"] = string(yamlBytes)
			}
		}
		writeJSON(w, resp)
		return
	}

	if r.Method == http.MethodPost {
		var req struct {
			RawYaml          string  `json:"rawYaml,omitempty"`
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}

		if req.RawYaml != "" {
			if s.configPath == "" {
				http.Error(w, `{"error":"config path not specified"}`, http.StatusBadRequest)
				return
			}
			newCfg, err := config.SyncRawYamlToConfigFile(s.configPath, req.RawYaml)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadRequest)
				return
			}
			s.cfgMu.Lock()
			s.cfg.RDP = newCfg.RDP
			s.cfg.Security.DefaultPolicy = newCfg.Security.DefaultPolicy
			s.cfg.Security.AllowAllControllerTargets = newCfg.Security.AllowAllControllerTargets
			s.cfg.Security.ControllerAccess = newCfg.Security.ControllerAccess
			if newCfg.Web.Token != "" {
				s.cfg.Web.Token = newCfg.Web.Token
			}
			s.cfgMu.Unlock()

			s.agentMgr.UpdateAccessPolicy(newCfg.Security.AllowAllControllerTargets, newCfg.Security.ControllerAccess)
			log.Printf("[RDPulse Web] Full relay config updated and saved to %s", s.configPath)
			writeJSON(w, map[string]string{"status": "updated"})
			return
		}

		// Structured settings update
		settings := config.ServerSettingsUpdate{
			PublicHost:       req.PublicHost,
			PortRangeStart:   req.PortRangeStart,
			PortRangeEnd:     req.PortRangeEnd,
			DefaultPolicy:    req.DefaultPolicy,
			WebToken:         req.WebToken,
			QuicListen:       req.QuicListen,
			TlsListen:        req.TlsListen,
			TlsDisabled:      req.TlsDisabled,
			RendezvousListen: req.RendezvousListen,
		}

		s.cfgMu.Lock()
		if req.PublicHost != nil {
			s.cfg.RDP.PublicHost = *req.PublicHost
		}
		if req.PortRangeStart != nil {
			s.cfg.RDP.PortRange.Start = uint16(*req.PortRangeStart)
		}
		if req.PortRangeEnd != nil {
			s.cfg.RDP.PortRange.End = uint16(*req.PortRangeEnd)
		}
		if req.DefaultPolicy != nil {
			s.cfg.Security.DefaultPolicy = *req.DefaultPolicy
		}
		if req.WebToken != nil {
			s.cfg.Web.Token = *req.WebToken
		}
		if req.QuicListen != nil {
			s.cfg.Server.QUIC.Listen = strings.TrimSpace(*req.QuicListen)
		}
		if req.TlsListen != nil {
			s.cfg.Server.TLS.Listen = strings.TrimSpace(*req.TlsListen)
		}
		if req.TlsDisabled != nil {
			s.cfg.Server.TLS.Disabled = *req.TlsDisabled
		}
		if req.RendezvousListen != nil {
			s.cfg.Server.Rendezvous.Listen = strings.TrimSpace(*req.RendezvousListen)
		}
		s.cfgMu.Unlock()

		if s.configPath != "" {
			if err := config.SyncServerSettingsToConfigFile(s.configPath, settings); err != nil {
				log.Printf("[RDPulse Web] Warning: failed to sync server settings to %s: %v", s.configPath, err)
			} else {
				log.Printf("[RDPulse Web] Synced server settings to %s", s.configPath)
			}
		}

		writeJSON(w, map[string]string{"status": "updated"})
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, history := Hub.Subscribe()
	defer Hub.Unsubscribe(ch)

	for _, entry := range history {
		data, _ := json.Marshal(entry)
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	flusher.Flush()

	notify := r.Context().Done()
	for {
		select {
		case <-notify:
			return
		case entry, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(entry)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func resolveInvitationExpiry(expiresAt string, expiresInDays int) (time.Time, error) {
	if strings.TrimSpace(expiresAt) != "" {
		t, err := parseExpiresAt(expiresAt)
		if err != nil {
			return time.Time{}, err
		}
		if !t.After(time.Now()) {
			return time.Time{}, fmt.Errorf("expiresAt must be in the future")
		}
		return t, nil
	}
	if expiresInDays <= 0 {
		expiresInDays = 30
	}
	return time.Now().Add(time.Duration(expiresInDays) * 24 * time.Hour), nil
}

func parseExpiresAt(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t, nil
		}
		if t, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid expiresAt")
}

func extractPort(addr, fallback string) string {
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		return addr[idx+1:]
	}
	return fallback
}

func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}
