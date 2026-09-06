//go:build !windows

package gui

func IsAutoStartEnabled() bool {
	return false
}

func SetAutoStartEnabled(enable bool) error {
	return nil
}
