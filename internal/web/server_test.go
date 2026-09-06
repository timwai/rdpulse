package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"rdpulse/internal/acl"
	"rdpulse/internal/config"
	"rdpulse/internal/relay"
	"rdpulse/internal/storage"
)

func TestWebServerEndpoints(t *testing.T) {
	db, err := storage.Open(t.TempDir() + "/web_test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := &config.RelayConfig{}
	cfg.Web.Listen = "127.0.0.1:18080"
	cfg.Web.Token = "secret-admin-token-12345"
	cfg.Server.QUIC.Listen = ":443"
	cfg.Server.Rendezvous.Listen = ":21116"
	cfg.RDP.PublicHost = "test.example.com"
	cfg.RDP.PortRange.Start = 20000
	cfg.RDP.PortRange.End = 20100
	cfg.Security.DefaultPolicy = "deny"
	cfg.Security.ControllerAccess = make(map[string][]string)

	aclMgr, _ := acl.NewManager("deny", nil)
	pm, _ := relay.NewPortManager(20000, 20100, db)
	agentMgr := relay.NewAgentManager(cfg, db, pm, aclMgr)
	defer agentMgr.Close()

	testCfgFile := t.TempDir() + "/test-relay.yaml"
	_ = os.WriteFile(testCfgFile, []byte("server:\n  quic:\n    allowEphemeralCertificate: true\nrdp:\n  publicHost: test.example.com\nsecurity:\n  defaultPolicy: deny\n  enrollmentInvitations: []\n"), 0644)

	srv := StartServer(context.Background(), cfg, testCfgFile, db, agentMgr)
	if srv == nil {
		t.Fatal("expected server to start")
	}
	defer srv.Shutdown(context.Background())

	handler := srv.httpServer.Handler
	withAuth := func(req *http.Request) *http.Request {
		req.Header.Set("Authorization", "Bearer secret-admin-token-12345")
		return req
	}

	// 1. Test Overview
	req := withAuth(httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("overview status: %d", w.Code)
	}

	// 2. Test Create Invitation
	body, _ := json.Marshal(map[string]interface{}{
		"deviceId":      "test-box",
		"expiresInDays": 7,
	})
	req = withAuth(httptest.NewRequest(http.MethodPost, "/api/invitations", bytes.NewReader(body)))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create invitation status: %d, body=%s", w.Code, w.Body.String())
	}
	var invResp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&invResp); err != nil {
		t.Fatal(err)
	}
	if invResp["deviceId"] != "test-box" || len(invResp["token"].(string)) < 32 {
		t.Fatalf("unexpected invitation response: %v", invResp)
	}

	// 3. Test List Invitations
	req = withAuth(httptest.NewRequest(http.MethodGet, "/api/invitations", nil))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list invitations status: %d", w.Code)
	}
	var invList []map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&invList); err != nil {
		t.Fatal(err)
	}
	if len(invList) != 1 || invList[0]["deviceId"] != "test-box" {
		t.Fatalf("expected test-box invitation, got %v", invList)
	}

	// 4. Test Access Policy Update
	policyBody, _ := json.Marshal(map[string]interface{}{
		"allowAll": true,
		"accessMap": map[string][]string{
			"test-box": {"my-laptop"},
		},
	})
	req = withAuth(httptest.NewRequest(http.MethodPost, "/api/access", bytes.NewReader(policyBody)))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("access update status: %d", w.Code)
	}

	allowAll, accessMap := agentMgr.GetAccessPolicy()
	if !allowAll || len(accessMap["test-box"]) != 1 {
		t.Fatalf("policy not updated in memory: allowAll=%v map=%v", allowAll, accessMap)
	}

	// 4.1 Verify that testCfgFile was updated with the access policy
	savedCfg, err := config.LoadRelayConfig(testCfgFile)
	if err != nil {
		t.Fatalf("failed to load saved config: %v", err)
	}
	if !savedCfg.Security.AllowAllControllerTargets {
		t.Errorf("expected AllowAllControllerTargets to be true in config file")
	}
	if len(savedCfg.Security.ControllerAccess["test-box"]) != 1 {
		t.Errorf("expected ControllerAccess to have test-box in config file")
	}

	// 4.2 Test GET /api/config
	req = withAuth(httptest.NewRequest(http.MethodGet, "/api/config", nil))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get config status: %d", w.Code)
	}
	var cfgResp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&cfgResp); err != nil {
		t.Fatal(err)
	}
	if cfgResp["publicHost"] != "test.example.com" || cfgResp["rawYaml"] == "" {
		t.Fatalf("unexpected config response: %v", cfgResp)
	}

	// 4.3 Test POST /api/config with basic settings
	settingsBody, _ := json.Marshal(map[string]interface{}{
		"publicHost":     "updated.example.com",
		"portRangeStart": 25000,
		"portRangeEnd":   25100,
		"defaultPolicy":  "allow",
	})
	req = withAuth(httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(settingsBody)))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update config status: %d, body=%s", w.Code, w.Body.String())
	}

	savedCfg, err = config.LoadRelayConfig(testCfgFile)
	if err != nil {
		t.Fatalf("failed to reload config file: %v", err)
	}
	if savedCfg.RDP.PublicHost != "updated.example.com" {
		t.Errorf("expected publicHost updated.example.com, got %s", savedCfg.RDP.PublicHost)
	}
	if savedCfg.RDP.PortRange.Start != 25000 || savedCfg.RDP.PortRange.End != 25100 {
		t.Errorf("expected port range 25000-25100, got %d-%d", savedCfg.RDP.PortRange.Start, savedCfg.RDP.PortRange.End)
	}

	listenBody, _ := json.Marshal(map[string]interface{}{
		"quicListen":       ":20000",
		"rendezvousListen": ":20001",
		"tlsListen":        ":20000",
		"tlsDisabled":      false,
	})
	req = withAuth(httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(listenBody)))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update listen config status: %d, body=%s", w.Code, w.Body.String())
	}
	if cfg.Server.QUIC.Listen != ":20000" || cfg.Server.Rendezvous.Listen != ":20001" {
		t.Fatalf("listen settings not applied in memory: quic=%s rdzv=%s", cfg.Server.QUIC.Listen, cfg.Server.Rendezvous.Listen)
	}

	future := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	expiryBody, _ := json.Marshal(map[string]string{"expiresAt": future})
	req = withAuth(httptest.NewRequest(http.MethodPatch, "/api/invitations/test-box", bytes.NewReader(expiryBody)))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("patch invitation expiry status: %d, body=%s", w.Code, w.Body.String())
	}
	invs, err := db.ListEnrollmentInvitations()
	if err != nil || len(invs) != 1 {
		t.Fatalf("list invitations after patch: %v err=%v", invs, err)
	}
	if invs[0].ExpiresAt.Sub(time.Now()) < 48*time.Hour {
		t.Fatalf("invitation expiry not extended: %v", invs[0].ExpiresAt)
	}

	// 5. Test Log Hub
	_, _ = Hub.Write([]byte("relay test log message"))

	// 6. Test Token Authentication
	cfg.Web.Token = "secret-admin-token-12345"
	// 6.1 Unauthorized request
	req = httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got: %d", w.Code)
	}

	// 6.2 Authorized request with Bearer token
	req = httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	req.Header.Set("Authorization", "Bearer secret-admin-token-12345")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK with Bearer token, got: %d", w.Code)
	}

	// 7. Test Invalid Device ID Injection in Invitation
	invalidBody, _ := json.Marshal(map[string]interface{}{
		"deviceId": "invalid device with spaces\nnewline",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/invitations", bytes.NewReader(invalidBody))
	req.Header.Set("Authorization", "Bearer secret-admin-token-12345")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for invalid deviceId, got: %d", w.Code)
	}

	// 8. Test Method Not Allowed on Actions
	req = httptest.NewRequest(http.MethodGet, "/api/devices/test-box/disconnect", nil)
	req.Header.Set("Authorization", "Bearer secret-admin-token-12345")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed on GET for disconnect, got: %d", w.Code)
	}

	// 9. Query token must not authenticate
	req = httptest.NewRequest(http.MethodGet, "/api/overview?token=secret-admin-token-12345", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for query token, got: %d", w.Code)
	}

	// 10. Cookie set by Bearer can authenticate EventSource-style requests
	req = httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	req.AddCookie(&http.Cookie{Name: webAuthCookie, Value: "secret-admin-token-12345"})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with auth cookie, got: %d", w.Code)
	}
}

func TestWebStartServerRefusesEmptyToken(t *testing.T) {
	cfg := &config.RelayConfig{}
	cfg.Web.Listen = "127.0.0.1:0"
	cfg.Web.Token = ""
	cfg.RDP.PublicHost = "test.example.com"

	srv := StartServer(context.Background(), cfg, "", nil, nil)
	if srv != nil {
		_ = srv.Shutdown(context.Background())
		t.Fatal("web console started without an admin token")
	}
}
