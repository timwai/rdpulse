package gui

import (
	"os/exec"
	"strings"
	"unsafe"

	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

type trayMenuState struct {
	DeviceID          string
	WindowVisible     bool
	TargetRunning     bool
	ControllerRunning bool
	AutoStart         bool
	ThemeDark         bool
}

type trayMenuItem struct {
	ID       uint32
	Text     string
	Sep      bool
	Disabled bool
	Checked  bool
	Default  bool
}

type trayMenuModel struct {
	Items []trayMenuItem
}

func buildTrayMenuModel(s trayMenuState) trayMenuModel {
	device := strings.TrimSpace(s.DeviceID)
	if device == "" {
		device = "未配置"
	}

	targetStatus := "已停止"
	if s.TargetRunning {
		targetStatus = "运行中"
	}
	sessionStatus := "未连接"
	if s.ControllerRunning {
		sessionStatus = "已连接"
	}

	showText := "打开主界面"
	if s.WindowVisible {
		showText = "隐藏到托盘"
	}
	targetText := "启动本机被控"
	if s.TargetRunning {
		targetText = "停止本机被控"
	}
	themeText := "深色界面"
	if !s.ThemeDark {
		themeText = "浅色界面"
	}

	return trayMenuModel{Items: []trayMenuItem{
		{Text: "RDPulse  ·  " + device, Disabled: true},
		{Text: "本机被控：" + targetStatus + "    远程会话：" + sessionStatus, Disabled: true},
		{Sep: true},
		{ID: TRAY_MENU_SHOW, Text: showText, Default: !s.WindowVisible},
		{Sep: true},
		{ID: TRAY_MENU_TARGET, Text: targetText},
		{ID: TRAY_MENU_DISCONNECT, Text: "断开远程会话", Disabled: !s.ControllerRunning},
		{ID: TRAY_MENU_COPY_ID, Text: "复制设备 ID"},
		{Sep: true},
		{ID: TRAY_MENU_AUTOSTART, Text: "开机自启", Checked: s.AutoStart},
		{ID: TRAY_MENU_THEME, Text: themeText, Checked: true},
		{ID: TRAY_MENU_CONFIG_DIR, Text: "打开配置目录"},
		{Sep: true},
		{ID: TRAY_MENU_EXIT, Text: "退出"},
	}}
}

func populateTrayMenu(hMenu win.HMENU, model trayMenuModel) {
	info := win.MENUINFO{
		CbSize:  uint32(unsafe.Sizeof(win.MENUINFO{})),
		FMask:   win.MIM_STYLE,
		DwStyle: win.MNS_CHECKORBMP,
	}
	win.SetMenuInfo(hMenu, &info)
	for _, it := range model.Items {
		appendTrayMenuItem(hMenu, it)
	}
}

func appendTrayMenuItem(hMenu win.HMENU, it trayMenuItem) {
	pos := uint32(win.GetMenuItemCount(hMenu))
	item := win.MENUITEMINFO{
		CbSize: uint32(unsafe.Sizeof(win.MENUITEMINFO{})),
	}
	if it.Sep {
		item.FMask = win.MIIM_FTYPE
		item.FType = win.MFT_SEPARATOR
		win.InsertMenuItem(hMenu, pos, true, &item)
		return
	}

	ptr, err := windows.UTF16PtrFromString(it.Text)
	if err != nil {
		return
	}
	item.FMask = win.MIIM_STRING | win.MIIM_ID | win.MIIM_STATE | win.MIIM_FTYPE
	item.FType = win.MFT_STRING
	item.WID = it.ID
	item.DwTypeData = ptr
	if it.Disabled {
		item.FState |= win.MFS_DISABLED
	}
	if it.Checked {
		item.FState |= win.MFS_CHECKED
	}
	if it.Default {
		item.FState |= win.MFS_DEFAULT
	}
	win.InsertMenuItem(hMenu, pos, true, &item)
}

func copyTextToClipboard(text string) {
	cmd := exec.Command("clip")
	cmd.Stdin = strings.NewReader(text)
	_ = cmd.Run()
}
