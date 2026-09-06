package gui

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"rdpulse/internal/agent"
	"rdpulse/internal/config"
	"rdpulse/internal/controller"
	"rdpulse/internal/path"
	"rdpulse/internal/service"

	"github.com/jchv/go-webview2"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
	"gopkg.in/yaml.v3"
)

var (
	oldWndProc uintptr
	globalApp  *App
)

const (
	WM_TRAY_CALLBACK     = win.WM_USER + 1024
	TRAY_MENU_SHOW       = 1001
	TRAY_MENU_TARGET     = 1002
	TRAY_MENU_EXIT       = 1003
	TRAY_MENU_DISCONNECT = 1004
	TRAY_MENU_COPY_ID    = 1005
	TRAY_MENU_AUTOSTART  = 1006
	TRAY_MENU_CONFIG_DIR = 1007
	TRAY_MENU_THEME      = 1008
)

type App struct {
	configPath string
	cfg        *config.AgentConfig

	w    webview2.WebView
	hwnd win.HWND

	startMinimized  bool
	forceExit       bool
	hasShownTrayTip bool

	// Controller state
	ctrlMu            sync.Mutex
	controllerRunning bool
	controllerCancel  context.CancelFunc
	controllerProxy   *controller.LocalProxy
	controllerPathMgr *path.Manager

	// Target state
	targetMu      sync.Mutex
	targetRunning bool
	targetCancel  context.CancelFunc

	// Real-time in-memory log buffer
	logMu      sync.Mutex
	logHistory []string
}

type uiLogWriter struct {
	app *App
}

// bestEffortWriter swallows write errors so a broken stderr (typical for
// -H=windowsgui binaries) cannot abort the rest of a MultiWriter chain.
type bestEffortWriter struct{ w io.Writer }

func (w bestEffortWriter) Write(p []byte) (int, error) {
	if w.w != nil {
		_, _ = w.w.Write(p)
	}
	return len(p), nil
}

func newGUILogOutput(app *App, console io.Writer) io.Writer {
	return io.MultiWriter(&uiLogWriter{app: app}, bestEffortWriter{console})
}

func (w *uiLogWriter) Write(p []byte) (n int, err error) {
	str := string(p)
	if w.app != nil {
		lines := strings.Split(str, "\n")
		for _, line := range lines {
			trimmed := strings.TrimRight(line, "\r")
			if strings.TrimSpace(trimmed) == "" {
				continue
			}
			w.app.logMu.Lock()
			if len(w.app.logHistory) >= 2000 {
				w.app.logHistory = w.app.logHistory[1:]
			}
			w.app.logHistory = append(w.app.logHistory, trimmed)
			w.app.logMu.Unlock()

			w.app.notifyLog(trimmed)
		}
	}
	return len(p), nil
}

func wndProcCallback(hwnd, msg, wp, lp uintptr) uintptr {
	if globalApp != nil {
		if msg == win.WM_CLOSE {
			if globalApp.cfg != nil && globalApp.cfg.GUI.MinimizeToTray && !globalApp.forceExit {
				win.ShowWindow(win.HWND(hwnd), win.SW_HIDE)
				if !globalApp.hasShownTrayTip {
					globalApp.hasShownTrayTip = true
					globalApp.showTrayBalloon("RDPulse 客户端已最小化", "程序仍在系统托盘后台运行中。\n双击托盘图标重新打开主界面，右键可退出程序。")
				}
				return 0
			}
			globalApp.cleanup()
		} else if msg == WM_TRAY_CALLBACK {
			switch lp {
			case win.WM_LBUTTONDOWN, win.WM_LBUTTONDBLCLK:
				if win.IsWindowVisible(win.HWND(hwnd)) {
					win.ShowWindow(win.HWND(hwnd), win.SW_HIDE)
				} else {
					win.ShowWindow(win.HWND(hwnd), win.SW_RESTORE)
					win.SetForegroundWindow(win.HWND(hwnd))
				}
			case win.WM_RBUTTONUP:
				globalApp.showTrayContextMenu()
			}
			return 0
		}
	}
	return win.CallWindowProc(oldWndProc, win.HWND(hwnd), uint32(msg), wp, lp)
}

func RunApp(configPath string, startMinimized bool) {
	release, ok := tryAcquireInstance()
	if !ok {
		activateExistingInstance()
		return
	}
	defer release()

	if configPath == "" {
		configPath = config.DefaultAgentConfigPath()
	}

	app := &App{
		configPath:     configPath,
		startMinimized: startMinimized,
	}
	globalApp = app

	log.SetOutput(newGUILogOutput(app, os.Stderr))

	app.ensureConfigFile()

	// Ensure WebView2 has a dedicated, writable profile data directory in %LOCALAPPDATA%\RDPulse\webview2
	// This avoids E_ACCESSDENIED when launching on system boot with CWD = C:\Windows\system32
	dataDir := filepath.Join(os.Getenv("LOCALAPPDATA"), "RDPulse", "webview2")
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".rdpulse", "webview2")
	}
	_ = os.MkdirAll(dataDir, 0755)

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		DataPath:  dataDir,
		WindowOptions: webview2.WindowOptions{
			Title:  "RDPulse 远程桌面穿透",
			Width:  880,
			Height: 680,
			Center: true,
			IconId: 1,
		},
	})
	if w == nil {
		log.Fatalf("Create WebView2 failed")
	}
	app.w = w
	app.hwnd = win.HWND(w.Window())

	// Apply Window Icons (Titlebar & Taskbar preview)
	hBig := getAppIcon(32)
	hSmall := getAppIcon(16)
	if hBig != 0 {
		win.SendMessage(app.hwnd, win.WM_SETICON, 1 /* ICON_BIG */, uintptr(hBig))
	}
	if hSmall != 0 {
		win.SendMessage(app.hwnd, win.WM_SETICON, 0 /* ICON_SMALL */, uintptr(hSmall))
	}

	// Subclass WNDPROC for close interception and system tray messages
	cb := syscall.NewCallback(wndProcCallback)
	oldWndProc = win.SetWindowLongPtr(app.hwnd, win.GWLP_WNDPROC, cb)

	// Register System Tray Icon
	app.setupTrayIcon()

	// Register Bidirectional IPC Bindings
	app.registerBindings(w)

	// Load embedded HTML with inlined tailwind.js for 100% offline standalone operation
	htmlBytes, err := assets.ReadFile("assets/index.html")
	if err != nil {
		log.Fatalf("Read embedded index.html failed: %v", err)
	}
	htmlStr := string(htmlBytes)
	if iconPNG, err := assets.ReadFile("assets/icon.png"); err == nil && len(iconPNG) > 0 {
		htmlStr = strings.ReplaceAll(htmlStr, `src="icon.png"`, `src="data:image/png;base64,`+base64.StdEncoding.EncodeToString(iconPNG)+`"`)
	}
	if tailwindBytes, err := assets.ReadFile("assets/tailwind.js"); err == nil {
		htmlStr = strings.Replace(htmlStr, `<script src="tailwind.js"></script>`, `<script>`+string(tailwindBytes)+`</script>`, 1)
	}
	if app.cfg != nil && app.cfg.GUI.Theme == "light" {
		htmlStr = strings.Replace(htmlStr, `<html lang="zh-CN" class="dark">`, `<html lang="zh-CN">`, 1)
	}
	w.SetHtml(htmlStr)

	if app.cfg != nil {
		log.Printf("[RDPulse GUI] 客户端已启动 minimized=%v 设备=%s 中继=%s Rendezvous=%s 本机RDP=%s 禁用P2P=%v",
			startMinimized, app.cfg.Device.ID, app.cfg.Server.Address, app.cfg.Server.RendezvousAddress, app.cfg.RDP.Address, app.cfg.Transport.DisableP2P)
	} else {
		log.Printf("[RDPulse GUI] 客户端已启动 minimized=%v", startMinimized)
	}
	log.Printf("[RDPulse GUI] 配置文件路径: %s", app.configPath)

	// Auto start target if configured
	if app.cfg != nil && app.cfg.GUI.AutoStartTarget {
		log.Println("[RDPulse GUI] 根据配置自动开启本机被控监听...")
		go func() {
			if err := app.onTargetStart(); err != nil {
				log.Printf("[RDPulse GUI] 自动开启被控监听失败: %v", err)
			}
		}()
	}

	// If startMinimized is requested, hide immediately
	if startMinimized {
		win.ShowWindow(app.hwnd, win.SW_HIDE)
	}

	w.Run()
	app.cleanup()
}

func (app *App) registerBindings(w webview2.WebView) {
	// 1. goGetConfig
	_ = w.Bind("goGetConfig", func() (string, error) {
		app.ensureConfigFile()
		payload := map[string]interface{}{
			"configPath":        app.configPath,
			"cfg":               app.cfg,
			"isAutostart":       IsAutoStartEnabled(),
			"targetRunning":     app.isTargetRunning(),
			"controllerRunning": app.isControllerRunning(),
		}
		data, err := json.Marshal(payload)
		return string(data), err
	})

	// 2. goSaveConfig
	_ = w.Bind("goSaveConfig", func(jsonStr string) (string, error) {
		type incomingCfg struct {
			Server struct {
				Address           string `json:"address"`
				RendezvousAddress string `json:"rendezvousAddress"`
			} `json:"server"`
			Device struct {
				ID              string `json:"id"`
				Secret          string `json:"secret"`
				EnrollmentToken string `json:"enrollmentToken"`
			} `json:"device"`
			RDP struct {
				Address string `json:"address"`
			} `json:"rdp"`
			GUI struct {
				AutoStartTarget bool   `json:"autoStartTarget"`
				MinimizeToTray  bool   `json:"minimizeToTray"`
				Theme           string `json:"theme"`
			} `json:"gui"`
		}

		var in incomingCfg
		if err := json.Unmarshal([]byte(jsonStr), &in); err != nil {
			return "解析配置失败: " + err.Error(), nil
		}

		serverAddr := strings.TrimSpace(in.Server.Address)
		if serverAddr == "" {
			return "中继服务器地址不能为空", nil
		}
		deviceID := strings.TrimSpace(in.Device.ID)
		if deviceID == "" {
			return "设备 Device ID 不能为空", nil
		}
		secret := strings.TrimSpace(in.Device.Secret)
		if len(secret) < 32 {
			return "设备 Secret 密钥长度至少需 32 字节", nil
		}

		if app.cfg == nil {
			app.cfg = config.DefaultAgentConfig()
		}
		app.cfg.Server.Address = serverAddr
		if app.cfg.Server.TLSAddress == "" {
			app.cfg.Server.TLSAddress = serverAddr
		}
		app.cfg.Server.RendezvousAddress = strings.TrimSpace(in.Server.RendezvousAddress)
		app.cfg.Device.ID = deviceID
		app.cfg.Device.Secret = secret
		app.cfg.Device.EnrollmentToken = strings.TrimSpace(in.Device.EnrollmentToken)
		if in.RDP.Address != "" {
			app.cfg.RDP.Address = strings.TrimSpace(in.RDP.Address)
		} else if app.cfg.RDP.Address == "" {
			app.cfg.RDP.Address = "127.0.0.1:3389"
		}
		app.cfg.GUI.AutoStartTarget = in.GUI.AutoStartTarget
		app.cfg.GUI.MinimizeToTray = in.GUI.MinimizeToTray
		if in.GUI.Theme == "light" || in.GUI.Theme == "dark" {
			app.cfg.GUI.Theme = in.GUI.Theme
		}
		app.cfg.SetDefaults()

		data, err := yaml.Marshal(app.cfg)
		if err != nil {
			return "序列化 YAML 失败: " + err.Error(), nil
		}

		dir := filepath.Dir(app.configPath)
		_ = os.MkdirAll(dir, 0755)
		if err := os.WriteFile(app.configPath, data, 0644); err != nil {
			return "写入配置文件失败: " + err.Error(), nil
		}
		log.Printf("[RDPulse GUI] 配置文件已成功保存至: %s", app.configPath)
		return "ok", nil
	})

	_ = w.Bind("goSetTheme", func(theme string) (string, error) {
		app.persistGUITheme(theme)
		return "ok", nil
	})

	// 3. goOpenConfigDir
	_ = w.Bind("goOpenConfigDir", func() {
		dir := filepath.Dir(app.configPath)
		_ = exec.Command("explorer", dir).Start()
	})

	// 4. goStartTarget
	_ = w.Bind("goStartTarget", func() (string, error) {
		if err := app.onTargetStart(); err != nil {
			return err.Error(), nil
		}
		return "ok", nil
	})

	// 5. goStopTarget
	_ = w.Bind("goStopTarget", func() {
		app.onTargetStop()
	})

	// 6. goConnect
	_ = w.Bind("goConnect", func(targetID, proxyAddr string, autoMstsc, disableP2P bool) (string, error) {
		if err := app.onControllerConnect(targetID, proxyAddr, autoMstsc, disableP2P); err != nil {
			return err.Error(), nil
		}
		return "ok", nil
	})

	// 7. goDisconnect
	_ = w.Bind("goDisconnect", func() {
		app.onControllerDisconnect()
	})

	// 8. goSetAutostart
	_ = w.Bind("goSetAutostart", func(enabled bool) (bool, error) {
		err := SetAutoStartEnabled(enabled)
		return err == nil, err
	})

	// 9. goCheck3389
	_ = w.Bind("goCheck3389", func() bool {
		return app.checkLocalRDPStatus()
	})

	// 10. goCopyClipboard
	_ = w.Bind("goCopyClipboard", func(text string) {
		copyTextToClipboard(text)
	})

	// 11. goMinimizeWindow
	_ = w.Bind("goMinimizeWindow", func() {
		win.ShowWindow(app.hwnd, win.SW_MINIMIZE)
	})

	// 12. goCloseWindow
	_ = w.Bind("goCloseWindow", func() {
		win.SendMessage(app.hwnd, win.WM_CLOSE, 0, 0)
	})

	// 13. goServiceAction
	_ = w.Bind("goServiceAction", func(act string) (string, error) {
		switch act {
		case "install":
			if err := service.Install(app.configPath); err != nil {
				return "安装失败: " + err.Error(), nil
			}
			return "服务安装成功", nil
		case "start":
			if err := service.Start(); err != nil {
				return "启动失败: " + err.Error(), nil
			}
			return "服务已启动", nil
		case "stop":
			if err := service.Stop(); err != nil {
				return "停止失败: " + err.Error(), nil
			}
			return "服务已停止", nil
		case "uninstall":
			if err := service.Uninstall(); err != nil {
				return "卸载失败: " + err.Error(), nil
			}
			return "服务已卸载", nil
		}
		return "未知操作", nil
	})

	// 14. goGetLogs
	_ = w.Bind("goGetLogs", func() (string, error) {
		app.logMu.Lock()
		defer app.logMu.Unlock()
		res := make([]string, len(app.logHistory))
		copy(res, app.logHistory)
		data, err := json.Marshal(res)
		return string(data), err
	})

	// 15. goClearLogs
	_ = w.Bind("goClearLogs", func() {
		app.logMu.Lock()
		app.logHistory = nil
		app.logMu.Unlock()
	})
}

func (app *App) notifyLog(msg string) {
	if app.w == nil {
		return
	}
	b, _ := json.Marshal(msg)
	app.w.Dispatch(func() {
		defer func() { _ = recover() }()
		if app.w != nil {
			app.w.Eval(fmt.Sprintf("window.onGoLog && window.onGoLog(%s)", string(b)))
		}
	})
}

func (app *App) notifyStatus(tcpPath, udpPath string, targetRunning, controllerRunning bool) {
	if app.w == nil {
		return
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"tcpPath":           tcpPath,
		"udpPath":           udpPath,
		"targetRunning":     targetRunning,
		"controllerRunning": controllerRunning,
	})
	app.w.Dispatch(func() {
		defer func() { _ = recover() }()
		if app.w != nil {
			app.w.Eval(fmt.Sprintf("window.onGoStatus && window.onGoStatus(%s)", string(payload)))
		}
	})
}

func (app *App) isTargetRunning() bool {
	app.targetMu.Lock()
	defer app.targetMu.Unlock()
	return app.targetRunning
}

func (app *App) isControllerRunning() bool {
	app.ctrlMu.Lock()
	defer app.ctrlMu.Unlock()
	return app.controllerRunning
}

func (app *App) ensureConfigFile() {
	dir := filepath.Dir(app.configPath)
	_ = os.MkdirAll(dir, 0755)

	cfg, err := config.LoadAgentConfig(app.configPath)
	if err == nil && cfg != nil {
		app.cfg = cfg
		return
	}

	defaultCfg := config.DefaultAgentConfig()
	defaultCfg.Server.Address = "relay.example.com:443"
	defaultCfg.Server.TLSAddress = "relay.example.com:443"
	defaultCfg.Server.RendezvousAddress = "relay.example.com:21116"
	defaultCfg.RDP.Address = "127.0.0.1:3389"
	defaultCfg.GUI.MinimizeToTray = true
	defaultCfg.GUI.AutoStartTarget = false

	hostName, _ := os.Hostname()
	if hostName != "" {
		defaultCfg.Device.ID = strings.ToLower(hostName)
	} else {
		defaultCfg.Device.ID = "my-pc"
	}

	b := make([]byte, 24)
	_, _ = rand.Read(b)
	defaultCfg.Device.Secret = hex.EncodeToString(b)

	app.cfg = defaultCfg
	if data, err := yaml.Marshal(defaultCfg); err == nil {
		_ = os.WriteFile(app.configPath, data, 0644)
	}
}

func (app *App) onControllerConnect(targetID, proxyAddr string, autoMstsc, disableP2P bool) error {
	app.ctrlMu.Lock()
	if app.controllerRunning {
		app.ctrlMu.Unlock()
		return fmt.Errorf("已有正在运行的连接会话")
	}

	cfg, err := config.LoadAgentConfig(app.configPath)
	if err != nil {
		app.ctrlMu.Unlock()
		return fmt.Errorf("读取配置失败: %w", err)
	}

	if disableP2P {
		cfg.Transport.DisableP2P = true
	}
	if proxyAddr == "" {
		proxyAddr = "127.0.0.1:13389"
	}

	ctx, cancel := context.WithCancel(context.Background())
	app.controllerCancel = cancel
	app.controllerRunning = true
	app.ctrlMu.Unlock()

	log.Printf("[Controller] 正在发起连接目标: %s, 本地代理: %s, 禁用P2P=%v", targetID, proxyAddr, disableP2P)

	client := agent.NewClient(cfg)
	proxy, pathMgr, err := client.ConnectTarget(ctx, targetID, proxyAddr, autoMstsc)
	if err != nil {
		log.Printf("[Controller] 连接失败: %v", err)
		app.ctrlMu.Lock()
		app.controllerRunning = false
		if app.controllerCancel != nil {
			app.controllerCancel()
			app.controllerCancel = nil
		}
		app.ctrlMu.Unlock()
		app.notifyStatus("-", "-", app.isTargetRunning(), false)
		return fmt.Errorf("连接失败: %w", err)
	}

	app.ctrlMu.Lock()
	app.controllerProxy = proxy
	app.controllerPathMgr = pathMgr
	app.ctrlMu.Unlock()

	log.Printf("[Controller] 会话建立成功！本地代理已监听: %s", proxy.Addr())
	log.Printf("[Controller] 链路状态: TCP=%s, UDP=%s", pathMgr.TCPPath(), pathMgr.UDPPath())
	app.notifyStatus(pathMgr.TCPPath().String(), pathMgr.UDPPath().String(), app.isTargetRunning(), true)

	go func() {
		defer func() {
			app.ctrlMu.Lock()
			app.controllerRunning = false
			if app.controllerCancel != nil {
				app.controllerCancel()
				app.controllerCancel = nil
			}
			if app.controllerProxy != nil {
				_ = app.controllerProxy.Close()
				app.controllerProxy = nil
			}
			if app.controllerPathMgr != nil {
				_ = app.controllerPathMgr.Close()
				app.controllerPathMgr = nil
			}
			app.ctrlMu.Unlock()
			app.notifyStatus("-", "-", app.isTargetRunning(), false)
			log.Println("[Controller] 远程会话已结束。")
		}()

		<-ctx.Done()
	}()

	return nil
}

func (app *App) onControllerDisconnect() {
	app.ctrlMu.Lock()
	cancel := app.controllerCancel
	proxy := app.controllerProxy
	pathMgr := app.controllerPathMgr
	app.controllerCancel = nil
	app.controllerProxy = nil
	app.controllerPathMgr = nil
	app.controllerRunning = false
	app.ctrlMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if proxy != nil {
		_ = proxy.Close()
	}
	if pathMgr != nil {
		_ = pathMgr.Close()
	}
	app.notifyStatus("-", "-", app.isTargetRunning(), false)
	log.Println("[Controller] 远程会话已断开。")
}

func (app *App) onTargetStart() error {
	app.targetMu.Lock()
	if app.targetRunning {
		app.targetMu.Unlock()
		return nil
	}

	cfg, err := config.LoadAgentConfig(app.configPath)
	if err != nil {
		app.targetMu.Unlock()
		return fmt.Errorf("读取配置失败: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	app.targetCancel = cancel
	app.targetRunning = true
	app.targetMu.Unlock()

	client := agent.NewClient(cfg)
	readyCh := make(chan error, 1)
	client.OnInitialConnect = func(err error) {
		select {
		case readyCh <- err:
		default:
		}
	}

	go func() {
		log.Printf("[Target] 前台被控端已启动，正在连接中继并开启 P2P 穿透监听...")
		_ = client.Run(ctx)
		app.targetMu.Lock()
		app.targetRunning = false
		app.targetCancel = nil
		app.targetMu.Unlock()
		app.notifyStatus("", "", false, app.isControllerRunning())
		log.Printf("[Target] 被控端已退出。")
	}()

	select {
	case err := <-readyCh:
		if err != nil {
			app.onTargetStop()
			return fmt.Errorf("连接中继失败: %w", err)
		}
		app.notifyStatus("", "", true, app.isControllerRunning())
		return nil
	case <-time.After(5 * time.Second):
		app.notifyStatus("", "", true, app.isControllerRunning())
		return nil
	}
}

func (app *App) onTargetStop() {
	app.targetMu.Lock()
	cancel := app.targetCancel
	app.targetCancel = nil
	app.targetRunning = false
	app.targetMu.Unlock()

	if cancel != nil {
		cancel()
	}
	app.notifyStatus("", "", false, app.isControllerRunning())
	log.Printf("[Target] 前台被控端已停止。")
}

func (app *App) checkLocalRDPStatus() bool {
	rdpAddr := "127.0.0.1:3389"
	if app.cfg != nil && app.cfg.RDP.Address != "" {
		rdpAddr = app.cfg.RDP.Address
	}

	conn, err := net.DialTimeout("tcp", rdpAddr, 300*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return true
	}
	return false
}

func getAppIcon(size int) win.HICON {
	// Prefer the ICO embedded in this package. Always rewrite the temp file:
	// a previous build left the old pulse/monitor icon in %TEMP% and never
	// refreshed it, so tray/title-bar kept showing the stale image.
	if icoBytes, err := assets.ReadFile("assets/icon.ico"); err == nil && len(icoBytes) > 0 {
		tmpPath := filepath.Join(os.TempDir(), "rdpulse_app_icon.ico")
		_ = os.WriteFile(tmpPath, icoBytes, 0644)
		var h win.HANDLE
		if size > 0 {
			h = win.LoadImage(0, windows.StringToUTF16Ptr(tmpPath), win.IMAGE_ICON, int32(size), int32(size), win.LR_LOADFROMFILE)
		} else {
			h = win.LoadImage(0, windows.StringToUTF16Ptr(tmpPath), win.IMAGE_ICON, 0, 0, win.LR_LOADFROMFILE|win.LR_DEFAULTSIZE)
		}
		if h != 0 {
			return win.HICON(h)
		}
	}

	if size > 0 {
		if h := win.LoadImage(win.GetModuleHandle(nil), win.MAKEINTRESOURCE(1), win.IMAGE_ICON, int32(size), int32(size), win.LR_SHARED); h != 0 {
			return win.HICON(h)
		}
	} else {
		if h := win.LoadImage(win.GetModuleHandle(nil), win.MAKEINTRESOURCE(1), win.IMAGE_ICON, 0, 0, win.LR_DEFAULTSIZE|win.LR_SHARED); h != 0 {
			return win.HICON(h)
		}
		if h := win.LoadIcon(win.GetModuleHandle(nil), win.MAKEINTRESOURCE(1)); h != 0 {
			return h
		}
	}

	return win.LoadIcon(0, win.MAKEINTRESOURCE(win.IDI_APPLICATION))
}

func trayIconSize() int {
	cx := int(win.GetSystemMetrics(win.SM_CXSMICON))
	if cx < 16 {
		return 16
	}
	return cx
}

func (app *App) setupTrayIcon() {
	hIcon := getAppIcon(trayIconSize())
	nid := win.NOTIFYICONDATA{
		CbSize:           uint32(unsafe.Sizeof(win.NOTIFYICONDATA{})),
		HWnd:             app.hwnd,
		UID:              1,
		UFlags:           win.NIF_MESSAGE | win.NIF_ICON | win.NIF_TIP,
		UCallbackMessage: WM_TRAY_CALLBACK,
		HIcon:            hIcon,
	}
	copy(nid.SzTip[:], windows.StringToUTF16("RDPulse 远程桌面穿透"))
	win.Shell_NotifyIcon(win.NIM_ADD, &nid)
}

func (app *App) removeTrayIcon() {
	nid := win.NOTIFYICONDATA{
		CbSize: uint32(unsafe.Sizeof(win.NOTIFYICONDATA{})),
		HWnd:   app.hwnd,
		UID:    1,
	}
	win.Shell_NotifyIcon(win.NIM_DELETE, &nid)
}

func (app *App) trayMenuState() trayMenuState {
	deviceID := ""
	themeDark := true
	if app.cfg != nil {
		deviceID = app.cfg.Device.ID
		themeDark = app.cfg.GUI.Theme != "light"
	}
	return trayMenuState{
		DeviceID:          deviceID,
		WindowVisible:     app.hwnd != 0 && win.IsWindowVisible(app.hwnd),
		TargetRunning:     app.isTargetRunning(),
		ControllerRunning: app.isControllerRunning(),
		AutoStart:         IsAutoStartEnabled(),
		ThemeDark:         themeDark,
	}
}

func (app *App) showTrayContextMenu() {
	var pt win.POINT
	win.GetCursorPos(&pt)
	hMenu := win.CreatePopupMenu()
	defer win.DestroyMenu(hMenu)

	populateTrayMenu(hMenu, buildTrayMenuModel(app.trayMenuState()))

	win.SetForegroundWindow(app.hwnd)
	cmd := win.TrackPopupMenu(
		hMenu,
		win.TPM_RETURNCMD|win.TPM_NONOTIFY|win.TPM_RIGHTBUTTON|win.TPM_BOTTOMALIGN|win.TPM_RIGHTALIGN,
		pt.X, pt.Y, 0, app.hwnd, nil,
	)
	win.PostMessage(app.hwnd, win.WM_NULL, 0, 0)
	app.handleTrayMenuCommand(int32(cmd))
}

func (app *App) handleTrayMenuCommand(cmd int32) {
	switch cmd {
	case TRAY_MENU_SHOW:
		if win.IsWindowVisible(app.hwnd) {
			win.ShowWindow(app.hwnd, win.SW_HIDE)
			return
		}
		win.ShowWindow(app.hwnd, win.SW_RESTORE)
		win.SetForegroundWindow(app.hwnd)
	case TRAY_MENU_TARGET:
		if app.isTargetRunning() {
			app.onTargetStop()
			return
		}
		if err := app.onTargetStart(); err != nil {
			app.showTrayBalloon("启动本机被控失败", err.Error())
		}
	case TRAY_MENU_DISCONNECT:
		app.onControllerDisconnect()
	case TRAY_MENU_COPY_ID:
		id := ""
		if app.cfg != nil {
			id = strings.TrimSpace(app.cfg.Device.ID)
		}
		if id == "" {
			app.showTrayBalloon("复制失败", "还没有配置设备 ID")
			return
		}
		copyTextToClipboard(id)
		app.showTrayBalloon("已复制设备 ID", id)
	case TRAY_MENU_AUTOSTART:
		next := !IsAutoStartEnabled()
		if err := SetAutoStartEnabled(next); err != nil {
			app.showTrayBalloon("开机自启", "设置失败："+err.Error())
			return
		}
		label := "未开启"
		if next {
			label = "已开启"
		}
		app.showTrayBalloon("开机自启", label)
		app.evalJS(`(function(){var el=document.getElementById('chk-autostart');var s=document.getElementById('side-autostart-status');if(el)el.checked=` + boolJS(next) + `;if(s){s.textContent=` + jsString(label) + `;}})()`)
	case TRAY_MENU_THEME:
		dark := true
		if app.cfg != nil {
			dark = app.cfg.GUI.Theme != "light"
		}
		next := "dark"
		if dark {
			next = "light"
		}
		app.persistGUITheme(next)
	case TRAY_MENU_CONFIG_DIR:
		dir := filepath.Dir(app.configPath)
		_ = exec.Command("explorer", dir).Start()
	case TRAY_MENU_EXIT:
		app.forceExit = true
		win.SendMessage(app.hwnd, win.WM_CLOSE, 0, 0)
	}
}

func boolJS(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (app *App) persistGUITheme(theme string) {
	if theme != "light" {
		theme = "dark"
	}
	if app.cfg == nil {
		app.ensureConfigFile()
	}
	if app.cfg == nil {
		return
	}
	app.cfg.GUI.Theme = theme
	if data, err := yaml.Marshal(app.cfg); err == nil {
		_ = os.MkdirAll(filepath.Dir(app.configPath), 0755)
		_ = os.WriteFile(app.configPath, data, 0644)
	}
	app.evalJS("applyTheme(" + boolJS(theme == "dark") + ", false)")
}

func (app *App) evalJS(script string) {
	if app.w == nil {
		return
	}
	app.w.Dispatch(func() {
		defer func() { _ = recover() }()
		if app.w != nil {
			app.w.Eval(script)
		}
	})
}

func (app *App) showTrayBalloon(title, info string) {
	nid := win.NOTIFYICONDATA{
		CbSize:      uint32(unsafe.Sizeof(win.NOTIFYICONDATA{})),
		HWnd:        app.hwnd,
		UID:         1,
		UFlags:      win.NIF_INFO,
		DwInfoFlags: 1, // NIIF_INFO
	}
	copy(nid.SzInfoTitle[:], windows.StringToUTF16(title))
	copy(nid.SzInfo[:], windows.StringToUTF16(info))
	win.Shell_NotifyIcon(win.NIM_MODIFY, &nid)
}

func (app *App) cleanup() {
	log.SetOutput(os.Stderr)
	app.removeTrayIcon()
	app.onTargetStop()
	app.onControllerDisconnect()
}
