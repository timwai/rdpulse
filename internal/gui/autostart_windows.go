//go:build windows

package gui

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const runRegKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runRegName = "RDPulseAgent"

// IsAutoStartEnabled checks if RDPulse is configured to start on user login in HKCU
func IsAutoStartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runRegKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()

	val, _, err := k.GetStringValue(runRegName)
	return err == nil && strings.TrimSpace(val) != ""
}

// SetAutoStartEnabled enables or disables startup on user login via HKCU Run key
func SetAutoStartEnabled(enable bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runRegKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	if enable {
		exePath, err := os.Executable()
		if err != nil {
			return err
		}
		exePath, _ = filepath.Abs(exePath)
		// Launch with --minimized so on user logon it starts quietly in the system tray
		cmd := `"` + exePath + `" --minimized`
		return k.SetStringValue(runRegName, cmd)
	}

	err = k.DeleteValue(runRegName)
	if err == registry.ErrNotExist {
		return nil
	}
	return err
}
