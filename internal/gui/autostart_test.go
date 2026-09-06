package gui

import (
	"testing"

	"golang.org/x/sys/windows/registry"
)

func TestAutoStartRegistry(t *testing.T) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runRegKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		t.Skipf("cannot open Run key: %v", err)
	}
	defer k.Close()

	origVal, _, origErr := k.GetStringValue(runRegName)
	defer func() {
		if origErr == nil {
			_ = k.SetStringValue(runRegName, origVal)
		} else {
			_ = k.DeleteValue(runRegName)
		}
	}()

	// Test enabling
	if err := SetAutoStartEnabled(true); err != nil {
		t.Fatalf("SetAutoStartEnabled(true) failed: %v", err)
	}
	if !IsAutoStartEnabled() {
		t.Errorf("Expected auto start to be enabled, but IsAutoStartEnabled returned false")
	}

	// Test disabling
	if err := SetAutoStartEnabled(false); err != nil {
		t.Fatalf("SetAutoStartEnabled(false) failed: %v", err)
	}
	if IsAutoStartEnabled() {
		t.Errorf("Expected auto start to be disabled, but IsAutoStartEnabled returned true")
	}
}
