// Package autostart starts a command when the user logs in, using what each
// OS provides: a systemd user service or XDG autostart entry on Linux, a
// LaunchAgent on macOS, and the Startup folder on Windows. Nothing is started
// immediately; it takes effect at the next login.
package autostart

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const name = "universal-portal"

// Entry is a program to run at login.
type Entry struct {
	Exe  string   // absolute path of the program
	Args []string // its arguments
	Home string   // user home directory (default: os.UserHomeDir)
	GOOS string   // default: runtime.GOOS
	// UseSystemd forces (true) or forbids (false) a systemd user service on
	// Linux; nil means use it when systemctl is available.
	UseSystemd *bool
}

func (e *Entry) home() string {
	if e.Home != "" {
		return e.Home
	}
	h, _ := os.UserHomeDir()
	return h
}

func (e *Entry) goos() string {
	if e.GOOS != "" {
		return e.GOOS
	}
	return runtime.GOOS
}

func (e *Entry) systemd() bool {
	if e.UseSystemd != nil {
		return *e.UseSystemd
	}
	_, err := exec.LookPath("systemctl")
	return err == nil
}

// paths returns the file that makes the entry start at login.
func (e *Entry) path() string {
	h := e.home()
	switch e.goos() {
	case "darwin":
		return filepath.Join(h, "Library", "LaunchAgents", "com.universal.portal.plist")
	case "windows":
		return filepath.Join(h, "AppData", "Roaming", "Microsoft", "Windows", "Start Menu", "Programs", "Startup", name+".cmd")
	default:
		if e.systemd() {
			return filepath.Join(h, ".config", "systemd", "user", name+".service")
		}
		return filepath.Join(h, ".config", "autostart", name+".desktop")
	}
}

func quote(s string) string {
	if strings.ContainsAny(s, " \t\"'") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// content returns the file body for the current OS.
func (e *Entry) content() string {
	argv := append([]string{e.Exe}, e.Args...)
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = quote(a)
	}
	cmdline := strings.Join(quoted, " ")
	switch e.goos() {
	case "darwin":
		var b strings.Builder
		for _, a := range argv {
			fmt.Fprintf(&b, "\t\t<string>%s</string>\n", xmlEscape(a))
		}
		return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.universal.portal</string>
	<key>ProgramArguments</key>
	<array>
` + b.String() + `	</array>
	<key>RunAtLoad</key>
	<true/>
</dict>
</plist>
`
	case "windows":
		return "@echo off\r\nstart \"Universal\" /min " + cmdline + "\r\n"
	default:
		if e.systemd() {
			return "[Unit]\nDescription=Universal portal\n\n[Service]\nExecStart=" + cmdline + "\nRestart=on-failure\n\n[Install]\nWantedBy=default.target\n"
		}
		return "[Desktop Entry]\nType=Application\nName=Universal portal\nExec=" + cmdline + "\nX-GNOME-Autostart-enabled=true\n"
	}
}

// Enabled reports whether the entry is installed.
func (e *Entry) Enabled() bool {
	_, err := os.Stat(e.path())
	return err == nil
}

// Enable installs the entry.
func (e *Entry) Enable() error {
	if e.Exe == "" {
		return errors.New("autostart: program path unknown")
	}
	p := e.path()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(e.content()), 0o644); err != nil {
		return err
	}
	if e.goos() == "linux" && e.systemd() && e.UseSystemd == nil {
		if out, err := exec.Command("systemctl", "--user", "enable", name+".service").CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl --user enable: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// Disable removes the entry.
func (e *Entry) Disable() error {
	if e.goos() == "linux" && e.systemd() && e.UseSystemd == nil {
		_ = exec.Command("systemctl", "--user", "disable", name+".service").Run()
	}
	err := os.Remove(e.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
