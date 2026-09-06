package gui

import (
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

const (
	guiInstanceMutexName = `Local\RDPulse.Agent.GUI`
	guiWindowTitle       = "RDPulse 远程桌面穿透"
	guiWindowClass       = "webview"
)

func tryAcquireInstance() (func(), bool) {
	return tryAcquireNamedInstance(guiInstanceMutexName)
}

func tryAcquireNamedInstance(name string) (release func(), ok bool) {
	mu, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return func() {}, false
	}
	handle, err := windows.CreateMutex(nil, false, mu)
	if handle == 0 {
		return func() {}, false
	}
	if err == windows.ERROR_ALREADY_EXISTS {
		_ = windows.CloseHandle(handle)
		return func() {}, false
	}
	return func() { _ = windows.CloseHandle(handle) }, true
}

func activateExistingInstance() {
	title, err := windows.UTF16PtrFromString(guiWindowTitle)
	if err != nil {
		return
	}
	class, err := windows.UTF16PtrFromString(guiWindowClass)
	if err != nil {
		return
	}

	hwnd := win.FindWindow(class, title)
	if hwnd == 0 {
		hwnd = win.FindWindow(nil, title)
	}
	if hwnd == 0 {
		return
	}

	win.ShowWindow(hwnd, win.SW_RESTORE)
	win.SetForegroundWindow(hwnd)
}
