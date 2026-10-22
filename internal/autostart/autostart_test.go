package autostart

import (
	"os"
	"strings"
	"testing"
)

func TestEntries(t *testing.T) {
	no, yes := false, true
	cases := []struct {
		goos    string
		systemd *bool
		file    string
		want    string
	}{
		{"linux", &no, ".config/autostart/universal-portal.desktop", `Exec="/opt/my apps/universal" portal`},
		{"linux", &yes, ".config/systemd/user/universal-portal.service", `ExecStart="/opt/my apps/universal" portal`},
		{"darwin", nil, "Library/LaunchAgents/com.universal.portal.plist", "<string>/opt/my apps/universal</string>"},
		{"windows", nil, "Startup/universal-portal.cmd", `start "Universal" /min "/opt/my apps/universal" portal`},
	}
	for _, c := range cases {
		home := t.TempDir()
		e := &Entry{Exe: "/opt/my apps/universal", Args: []string{"portal"}, Home: home, GOOS: c.goos, UseSystemd: c.systemd}
		if e.Enabled() {
			t.Fatalf("%s: enabled before Enable", c.goos)
		}
		if err := e.Enable(); err != nil {
			t.Fatalf("%s: %v", c.goos, err)
		}
		if !e.Enabled() || !strings.HasSuffix(strings.ReplaceAll(e.path(), "\\", "/"), c.file) {
			t.Fatalf("%s: path %s", c.goos, e.path())
		}
		b, _ := os.ReadFile(e.path())
		if !strings.Contains(string(b), c.want) {
			t.Fatalf("%s: content %q", c.goos, b)
		}
		if err := e.Disable(); err != nil || e.Enabled() {
			t.Fatalf("%s: disable: %v", c.goos, err)
		}
	}
}
