package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/TanmayDoesAI/tailreach/internal/server"
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
	server.RetireOldLoginItem()
	server.MigrateState()
	logFile := filepath.Join(server.DefaultStateDir(), "tailreach.log")
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

// uninstallCmd stops tailreach and takes it off this Mac. Homebrew can't remove a login
// item, so `brew uninstall` alone would leave the copy setup keeps running: this goes first.
// --all also forgets the Tailscale node, password, passkeys and the signing identity.
func uninstallCmd(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	all := fs.Bool("all", false, "also delete tailreach's data (Tailscale node, password, passkeys, logs) and its signing identity")
	fs.Parse(args)
	if err := uninstall(); err != nil {
		return err
	}
	if *all {
		if err := os.RemoveAll(server.DefaultStateDir()); err != nil {
			return err
		}
		fmt.Println("✓ deleted", server.DefaultStateDir())
		if exec.Command("security", "find-certificate", "-c", "tailreach local").Run() == nil {
			if out, err := exec.Command("security", "delete-identity", "-c", "tailreach local").CombinedOutput(); err != nil {
				fmt.Printf("! couldn't delete the signing identity \"tailreach local\" (%s): remove it in Keychain Access\n", strings.TrimSpace(string(out)))
			} else {
				fmt.Println("✓ deleted the signing identity \"tailreach local\"")
			}
		}
	}
	fmt.Println("\nLeft for you:")
	fmt.Println("  • System Settings → Privacy & Security → Screen Recording and Accessibility: select tailreach, click −")
	if cellar.MatchString(selfPath()) {
		fmt.Println("  • brew uninstall tailreach")
	}
	if *all {
		fmt.Println("  • remove the tailreach machine (\"devices\" or this Mac's name) in the Tailscale admin console")
	} else {
		fmt.Println("  • to forget its data and sign-in too: tailreach uninstall --all")
	}
	return nil
}

func uninstall() error {
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	exec.Command("launchctl", "bootout", domain+"/"+launchLabel).Run() // stops the menu-bar item with it
	if err := os.Remove(plistPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("✓ stopped tailreach and removed its login item")
	if _, err := os.Stat(installedDir()); err == nil {
		os.RemoveAll(installedDir())
		fmt.Println("✓ removed the copy setup installed in", installedDir())
	}
	return nil
}

func selfPath() string {
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	return exe
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
