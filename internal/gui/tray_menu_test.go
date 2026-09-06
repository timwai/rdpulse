package gui

import (
	"strings"
	"testing"
)

func itemByID(model trayMenuModel, id uint32) trayMenuItem {
	for _, it := range model.Items {
		if it.ID == id {
			return it
		}
	}
	return trayMenuItem{}
}

func TestBuildTrayMenuModelCoversCoreActions(t *testing.T) {
	model := buildTrayMenuModel(trayMenuState{
		DeviceID:           "office-pc",
		WindowVisible:      false,
		TargetRunning:      false,
		ControllerRunning:  false,
		AutoStart:          true,
		ThemeDark:          true,
	})

	var texts []string
	var hasSep bool
	for _, it := range model.Items {
		if it.Sep {
			hasSep = true
			continue
		}
		if strings.ContainsAny(it.Text, "🖥🛡❌🌙☀") || strings.Contains(it.Text, "️") {
			t.Fatalf("menu text should not use emoji: %q", it.Text)
		}
		texts = append(texts, it.Text)
	}
	if !hasSep {
		t.Fatal("expected separators between groups")
	}

	joined := strings.Join(texts, "\n")
	for _, want := range []string{"office-pc", "打开主界面", "启动本机被控", "断开远程会话", "复制设备 ID", "开机自启", "打开配置目录", "退出"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in menu:\n%s", want, joined)
		}
	}

	open := itemByID(model, TRAY_MENU_SHOW)
	if !open.Default || open.Disabled {
		t.Fatalf("打开主界面 should be the default enabled item: %+v", open)
	}
	if disconnect := itemByID(model, TRAY_MENU_DISCONNECT); !disconnect.Disabled {
		t.Fatal("disconnect should be disabled when no session")
	}
	if auto := itemByID(model, TRAY_MENU_AUTOSTART); !auto.Checked {
		t.Fatal("autostart should be checked")
	}
}

func TestBuildTrayMenuModelReflectsRunningState(t *testing.T) {
	model := buildTrayMenuModel(trayMenuState{
		DeviceID:          "office-pc",
		WindowVisible:     true,
		TargetRunning:     true,
		ControllerRunning: true,
		ThemeDark:         false,
	})

	if got := itemByID(model, TRAY_MENU_SHOW).Text; got != "隐藏到托盘" {
		t.Fatalf("show item = %q, want 隐藏到托盘", got)
	}
	if got := itemByID(model, TRAY_MENU_TARGET).Text; got != "停止本机被控" {
		t.Fatalf("target item = %q, want 停止本机被控", got)
	}
	if itemByID(model, TRAY_MENU_DISCONNECT).Disabled {
		t.Fatal("disconnect should be enabled during a session")
	}
	if got := itemByID(model, TRAY_MENU_THEME).Text; !strings.Contains(got, "浅色") {
		t.Fatalf("theme item = %q, want 浅色 when light theme is active", itemByID(model, TRAY_MENU_THEME).Text)
	}
}
