package gui

import (
	"runtime"
	"testing"
	"time"

	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

func readClipboardUnicode() (string, bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for i := 0; i < 15; i++ {
		if win.OpenClipboard(0) {
			h := win.GetClipboardData(win.CF_UNICODETEXT)
			if h != 0 {
				ptr := win.GlobalLock(win.HGLOBAL(h))
				if ptr != nil {
					got := windows.UTF16PtrToString((*uint16)(ptr))
					win.GlobalUnlock(win.HGLOBAL(h))
					win.CloseClipboard()
					return got, true
				}
			}
			win.CloseClipboard()
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "", false
}

func TestCopyTextToClipboardPreservesChinese(t *testing.T) {
	const text = "配置文件路径：中继已启动"
	if err := setClipboardUnicode(text); err != nil {
		t.Fatalf("setClipboardUnicode: %v", err)
	}
	got, ok := readClipboardUnicode()
	if !ok {
		t.Skip("clipboard is locked or unavailable")
	}
	if got != text {
		t.Fatalf("clipboard text garbled: got %q want %q", got, text)
	}
}

func TestClipboardUTF16IsNotRawUTF8(t *testing.T) {
	const text = "已启动"
	got, err := clipboardUTF16(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[len(got)-1] != 0 {
		t.Fatal("expected null-terminated UTF-16")
	}
	if windows.UTF16ToString(got) != text {
		t.Fatalf("round-trip failed: %q", windows.UTF16ToString(got))
	}
}
