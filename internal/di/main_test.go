package di

import (
	"fmt"
	"os"
	"testing"
)

// TestMain points the per-user config directory (os.UserConfigDir: AppData, XDG_CONFIG_HOME, HOME) at a temporary
// one for the whole package. The local-mode providers open the desktop app's settings file there; a test must never
// read, quarantine or rewrite the developer's own.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "baki-di-test-home")
	if err != nil {
		fmt.Fprintln(os.Stderr, "temp config home:", err)
		os.Exit(1)
	}
	for _, k := range []string{"AppData", "XDG_CONFIG_HOME", "HOME"} {
		if err := os.Setenv(k, home); err != nil {
			fmt.Fprintln(os.Stderr, "set", k, err)
			os.Exit(1)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
