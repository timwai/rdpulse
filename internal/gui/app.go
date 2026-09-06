package gui

import (
	"context"
	"crypto/rand"
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
	user32          = windows.NewLazySystemDLL("user32.dll")
	procAppendMenuW = user32.NewProc("AppendMenuW")
	oldWndProc      uintptr
	globalApp       *App
)

const (
	WM_TRAY_CALLBACK = win.WM_USER + 1024
	TRAY_MENU_SHOW   = 1001
	TRAY_MENU_TARGET = 1002
	TRAY_MENU_EXIT   = 1003
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
			if len(w.app.logHistory) >= 1000 {
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
	if configPath == "" {
		configPath = config.DefaultAgentConfigPath()
	}

	app := &App{
		configPath:     configPath,
		startMinimized: startMinimized,
	}
	globalApp = app

	uiWriter := &uiLogWriter{app: app}
	log.SetOutput(io.MultiWriter(os.Stderr, uiWriter))

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
	tailwindBytes, err := assets.ReadFile("assets/tailwind.js")
	if err == nil {
		htmlStr := string(htmlBytes)
		htmlStr = strings.Replace(htmlStr, `<script src="tailwind.js"></script>`, `<script>`+string(tailwindBytes)+`</script>`, 1)
		w.SetHtml(htmlStr)
	} else {
		w.SetHtml(string(htmlBytes))
	}

	// Seed initial log history
	app.logMu.Lock()
	app.logHistory = append(app.logHistory,
		fmt.Sprintf("[RDPulse GUI] 旗舰版原生客户端已启动 (minimized=%v)", startMinimized),
		fmt.Sprintf("[RDPulse GUI] 配置文件路径: %s", app.configPath),
	)
	app.logMu.Unlock()

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
				AutoStartTarget bool `json:"autoStartTarget"`
				MinimizeToTray  bool `json:"minimizeToTray"`
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
		cmd := exec.Command("clip")
		cmd.Stdin = strings.NewReader(text)
		_ = cmd.Run()
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

func (app *App) notifyStatus(tcpPath, udpPath string, targetRunning bool) {
	if app.w == nil {
		return
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"tcpPath":           tcpPath,
		"udpPath":           udpPath,
		"targetRunning":     targetRunning,
		"controllerRunning": app.isControllerRunning(),
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
		app.notifyStatus("-", "-", app.isTargetRunning())
		return fmt.Errorf("连接失败: %w", err)
	}

	app.ctrlMu.Lock()
	app.controllerProxy = proxy
	app.controllerPathMgr = pathMgr
	app.ctrlMu.Unlock()

	log.Printf("[Controller] 会话建立成功！本地代理已监听: %s", proxy.Addr())
	log.Printf("[Controller] 链路状态: TCP=%s, UDP=%s", pathMgr.TCPPath(), pathMgr.UDPPath())
	app.notifyStatus(pathMgr.TCPPath().String(), pathMgr.UDPPath().String(), app.isTargetRunning())

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
			app.notifyStatus("-", "-", app.isTargetRunning())
			log.Println("[Controller] 远程会话已结束。")
		}()

		<-ctx.Done()
	}()

	return nil
}

func (app *App) onControllerDisconnect() {
	app.ctrlMu.Lock()
	defer app.ctrlMu.Unlock()

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
	app.controllerRunning = false
	app.notifyStatus("-", "-", app.isTargetRunning())
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
		app.notifyStatus("", "", false)
		log.Printf("[Target] 被控端已退出。")
	}()

	select {
	case err := <-readyCh:
		if err != nil {
			app.onTargetStop()
			return fmt.Errorf("连接中继失败: %w", err)
		}
		app.notifyStatus("", "", true)
		return nil
	case <-time.After(5 * time.Second):
		app.notifyStatus("", "", true)
		return nil
	}
}

func (app *App) onTargetStop() {
	app.targetMu.Lock()
	defer app.targetMu.Unlock()

	if app.targetCancel != nil {
		app.targetCancel()
		app.targetCancel = nil
	}
	app.targetRunning = false
	app.notifyStatus("", "", false)
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
	// 1. Try loading from PE resource (embedded via rsrc.syso)
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

	// 2. Fallback: extract embedded assets/icon.ico to temp folder and load
	if icoBytes, err := assets.ReadFile("assets/icon.ico"); err == nil && len(icoBytes) > 0 {
		tmpPath := filepath.Join(os.TempDir(), "rdpulse_app_icon.ico")
		if _, err := os.Stat(tmpPath); err != nil {
			_ = os.WriteFile(tmpPath, icoBytes, 0644)
		}
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

	// 3. Ultimate fallback to standard Windows application icon
	return win.LoadIcon(0, win.MAKEINTRESOURCE(win.IDI_APPLICATION))
}

func (app *App) setupTrayIcon() {
	hIcon := getAppIcon(16)
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

func (app *App) showTrayContextMenu() {
	var pt win.POINT
	win.GetCursorPos(&pt)
	hMenu := win.CreatePopupMenu()
	defer win.DestroyMenu(hMenu)

	appendMenu(hMenu, 0, TRAY_MENU_SHOW, "🖥️ 显示主界面")
	appendMenu(hMenu, 0, TRAY_MENU_TARGET, "🛡️ 启停本机被控")
	appendMenu(hMenu, win.MF_SEPARATOR, 0, "")
	appendMenu(hMenu, 0, TRAY_MENU_EXIT, "❌ 退出程序")

	win.SetForegroundWindow(app.hwnd)
	cmd := win.TrackPopupMenu(hMenu, win.TPM_RETURNCMD|win.TPM_NONOTIFY, pt.X, pt.Y, 0, app.hwnd, nil)
	switch cmd {
	case TRAY_MENU_SHOW:
		win.ShowWindow(app.hwnd, win.SW_RESTORE)
		win.SetForegroundWindow(app.hwnd)
	case TRAY_MENU_TARGET:
		app.targetMu.Lock()
		running := app.targetRunning
		app.targetMu.Unlock()
		if running {
			app.onTargetStop()
		} else {
			_ = app.onTargetStart()
		}
	case TRAY_MENU_EXIT:
		app.forceExit = true
		win.SendMessage(app.hwnd, win.WM_CLOSE, 0, 0)
	}
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

func appendMenu(hMenu win.HMENU, flags uint32, id uintptr, text string) {
	if text == "" {
		procAppendMenuW.Call(uintptr(hMenu), uintptr(flags), id, 0)
	} else {
		ptr, _ := windows.UTF16PtrFromString(text)
		procAppendMenuW.Call(uintptr(hMenu), uintptr(flags), id, uintptr(unsafe.Pointer(ptr)))
	}
}

func (app *App) cleanup() {
	log.SetOutput(os.Stderr)
	app.removeTrayIcon()
	app.onTargetStop()
	app.onControllerDisconnect()
}
