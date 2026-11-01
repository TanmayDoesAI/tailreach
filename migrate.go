package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Before 0.2 tailreach was called mydevices. An install from then moves over once: its
// login item goes, and its settings folder (Tailscale login, password, passkeys, trusted
// devices) is renamed. macOS asks again for Screen Recording and Accessibility, which were
// granted to the old name.

const oldLaunchLabel = "com.mydevices.agent"

func oldPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", oldLaunchLabel+".plist")
}

// retireOldLoginItem stops and removes the old login item: two copies must never run on
// one Tailscale state.
func retireOldLoginItem() {
	if runtime.GOOS != "darwin" {
		return
	}
	if _, err := os.Stat(oldPlistPath()); err != nil {
		return
	}
	exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), oldLaunchLabel)).Run()
	os.Remove(oldPlistPath())
	fmt.Println("✓ removed the old mydevices login item")
}

// migrateState renames the old settings folder and what's in it, unless the old login item
// is still there (it would be using them). Safe to call every start.
func migrateState() {
	if _, err := os.Stat(oldPlistPath()); err == nil {
		return
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return
	}
	old, dir := filepath.Join(cfg, "mydevices"), defaultStateDir()
	if moveIfFree(old, dir) {
		log.Printf("✓ moved settings from %s to %s", old, dir)
	}
	moveIfFree(filepath.Join(dir, "mydevices"), filepath.Join(dir, "tailreach"))         // the hub's Tailscale state
	moveIfFree(filepath.Join(dir, "mydevices.log"), filepath.Join(dir, "tailreach.log")) // its log
}

func moveIfFree(from, to string) bool {
	if _, err := os.Stat(from); err != nil {
		return false
	}
	if _, err := os.Stat(to); err == nil {
		return false
	}
	return os.Rename(from, to) == nil
}
