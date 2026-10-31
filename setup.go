package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
)

// `mydevices setup` does on an installed copy (Homebrew's, say) what `make setup` does in a
// checkout: the signing identity, both binaries signed with it, the login item, then
// doctor. Safe to run again, and needed again after `brew upgrade`: Homebrew builds
// without access to the keychain, so its copies come ad-hoc signed, and macOS would
// forget their Screen Recording and Accessibility permissions.

//go:embed scripts/signing-identity.sh
var signingIdentityScript string

func setup(args []string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("setup is macOS-only; elsewhere run mydevices from your init system")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)

	if out, err := exec.Command("sh", "-c", signingIdentityScript).CombinedOutput(); err != nil {
		return fmt.Errorf("signing identity: %v: %s", err, out)
	} else {
		fmt.Print(string(out))
	}
	for _, b := range []struct{ path, id string }{{exe, "com.mydevices.agent"}, {helperPath(), "com.mydevices.capture"}} {
		if err := resign(b.path, b.id); err != nil {
			return fmt.Errorf("signing %s: %v", filepath.Base(b.path), err)
		}
	}
	fmt.Println("✓ signed with \"mydevices local\", so macOS keeps the permissions you grant")

	// An existing login item keeps its flags; one that runs this copy is just restarted.
	var was []string
	if out, err := exec.Command("plutil", "-extract", "ProgramArguments", "json", "-o", "-", plistPath()).Output(); err == nil {
		json.Unmarshal(out, &was)
	}
	if len(args) == 0 && len(was) > 0 {
		if was[0] == launchPath(exe) {
			exec.Command("launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchLabel)).Run()
			fmt.Println("✓ login item restarted")
			return doctor(nil)
		}
		args = was[1:]
	}
	if len(args) == 0 {
		args = []string{"--hub", "--name", "mac"}
	}
	if err := install(args); err != nil {
		return err
	}
	return doctor(nil)
}

// resign signs a copy and moves it into place: signing the file of a running program in
// place can get that program killed.
func resign(path, id string) error {
	tmp := path + ".signing"
	if out, err := exec.Command("cp", "-p", path, tmp).CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	if out, err := exec.Command("codesign", "--force", "-s", "mydevices local", "--identifier", id, tmp).CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%v: %s", err, out)
	}
	return os.Rename(tmp, path)
}

// launchPath is the path the login item should start: for a Homebrew copy, the stable
// opt/ link rather than this version's Cellar folder, so `brew upgrade` doesn't break it.
func launchPath(exe string) string {
	m := regexp.MustCompile(`^(.*)/Cellar/([^/]+)/[^/]+/(.+)$`).FindStringSubmatch(exe)
	if m == nil {
		return exe
	}
	opt := filepath.Join(m[1], "opt", m[2], m[3])
	if _, err := os.Stat(opt); err != nil {
		return exe
	}
	return opt
}
