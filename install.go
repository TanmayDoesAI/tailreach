package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const launchLabel = "com.tailreach.agent"

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchLabel+".plist")
}

// install registers tailreach to start at login (macOS LaunchAgent) with the given flags.
func install(args []string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("install is macOS-only for now; run tailreach from your init system instead")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	exe = launchPath(exe)
	retireOldLoginItem()
	migrateState()
	logFile := filepath.Join(defaultStateDir(), "tailreach.log")
	os.MkdirAll(filepath.Dir(logFile), 0o700)

	var argXML strings.Builder
	for _, a := range append([]string{exe}, args...) {
		fmt.Fprintf(&argXML, "\n    <string>%s</string>", xmlEscape(a))
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>%s
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchLabel, argXML.String(), xmlEscape(logFile), xmlEscape(logFile))

	os.MkdirAll(filepath.Dir(plistPath()), 0o755)
	if err := os.WriteFile(plistPath(), []byte(plist), 0o644); err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	exec.Command("launchctl", "bootout", domain+"/"+launchLabel).Run()
	if out, err := exec.Command("launchctl", "bootstrap", domain, plistPath()).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl: %v: %s", err, out)
	}
	fmt.Println("✓ tailreach will now start at login. Logs:", logFile)
	return nil
}

func uninstall() error {
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	exec.Command("launchctl", "bootout", domain+"/"+launchLabel).Run()
	if err := os.Remove(plistPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("✓ removed login item")
	return nil
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
