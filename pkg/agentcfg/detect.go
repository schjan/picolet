package agentcfg

import (
	"os"
	"runtime"
)

var detectSystemdUserFunc = detectSystemdUser

// DetectSystemdUser reports whether picolet should manage the user systemd
// instance based on the current process environment.
func DetectSystemdUser() bool {
	return detectSystemdUserFunc()
}

func detectSystemdUser() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if os.Geteuid() != 0 {
		return true
	}
	if _, err := os.Stat("/run/dbus/system_bus_socket"); err == nil {
		return false
	}
	return true
}
