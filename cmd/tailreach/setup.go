package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/TanmayDoesAI/tailreach/internal/server"
	"github.com/TanmayDoesAI/tailreach/scripts"
)

// `tailreach setup` does on an installed copy (Homebrew's, say) what `make setup` does in a
// checkout: the signing identity, both binaries signed with it, the login item, then
// doctor. Safe to run again, and needed again after `brew upgrade`: Homebrew builds
// without access to the keychain, so its copies come ad-hoc signed, and macOS would
// forget their Screen Recording and Accessibility permissions.
//
// A Homebrew copy is copied into installedDir() first, and the login item runs that: macOS
// files a command-line program's permissions by its path, and Homebrew puts every version
// in a new folder, so running it from there would add another "tailreach" to each Privacy
// list (and ask again) on every upgrade.

func setup(args []string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("setup is macOS-only; elsewhere run tailreach from your init system")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)

	if out, err := exec.Command("sh", "-c", scripts.SigningIdentity).CombinedOutput(); err != nil {
		return fmt.Errorf("couldn't make the signing identity (%v): %s\n"+
			"If macOS asked for your login keychain password, run `tailreach setup` again and allow it.", err, strings.TrimSpace(string(out)))
	} else {
		fmt.Print(string(out))
	}
	inst := launchPath(exe)
	if inst != exe {
		if err := os.MkdirAll(installedDir(), 0o755); err != nil {
			return err
		}
	}
	for _, b := range []struct{ src, dst, id string }{
		{exe, inst, "com.tailreach.agent"},
		{server.HelperPath(), installedHelper(exe), "com.tailreach.capture"},
	} {
		if err := signInto(b.src, b.dst, b.id); err != nil {
			return fmt.Errorf("signing %s: %v", filepath.Base(b.dst), err)
		}
	}
	if inst != exe {
		fmt.Println("✓ installed in", installedDir()+", the same place every version, so permissions survive upgrades")
	}
	fmt.Println("✓ signed with \"tailreach local\", so macOS keeps the permissions you grant")

	// An existing login item keeps its flags; one that runs this copy is just restarted.
	var was []string
	for _, p := range []string{plistPath(), server.OldPlistPath()} { // an install from before the rename keeps its flags too
		if out, err := exec.Command("plutil", "-extract", "ProgramArguments", "json", "-o", "-", p).Output(); err == nil {
			json.Unmarshal(out, &was)
			break
		}
	}
	if len(args) == 0 && len(was) > 0 {
		// Already this copy and loaded: a restart is enough. (A login item file that isn't
		// loaded, e.g. after an interrupted setup, is installed again below.)
		if was[0] == launchPath(exe) && exec.Command("launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchLabel)).Run() == nil {
			exec.Command("launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchLabel)).Run()
			fmt.Println("✓ login item restarted")
			grantPermissions(server.DefaultStateDir(), exe)
			return doctor(nil)
		}
		args = carryFlags(was[1:])
	}
	if len(args) == 0 {
		args = []string{"--hub", "--name", "mac"}
	}
	if err := install(args); err != nil {
		return err
	}
	grantPermissions(server.DefaultStateDir(), exe)
	return doctor(nil)
}

// signInto signs a copy of src and moves it into place at dst (which may be src itself):
// signing or overwriting the file of a running program in place can get it killed.
func signInto(src, dst, id string) error {
	tmp := dst + ".signing"
	if out, err := exec.Command("cp", "-p", src, tmp).CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	if out, err := exec.Command("codesign", "--force", "-s", "tailreach local", "--identifier", id, tmp).CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%v: %s", err, out)
	}
	return os.Rename(tmp, dst)
}

// carryFlags keeps an old login item's settings (name, hub, via…) but drops the ones only
// meant for working on tailreach itself: a copy migrated from a checkout must not keep
// serving its web pages from that checkout, or log verbosely forever.
func carryFlags(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := strings.TrimLeft(args[i], "-")
		name, _, inline := strings.Cut(a, "=")
		switch name {
		case "web-dir":
			if !inline {
				i++ // its value
			}
			continue
		case "v":
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// installedDir is where setup keeps the copy of a Homebrew install that the login item runs.
func installedDir() string { return filepath.Join(server.DefaultStateDir(), "bin") }

var cellar = regexp.MustCompile(`/Cellar/[^/]+/[^/]+/`)

// launchPath is the path the login item should start: for a Homebrew copy (in one version's
// Cellar folder), the copy in installedDir() that setup keeps, so the path never changes.
func launchPath(exe string) string {
	if !cellar.MatchString(exe) {
		return exe
	}
	return filepath.Join(installedDir(), filepath.Base(exe))
}

// installedHelper is the capture helper next to the copy the login item runs.
func installedHelper(exe string) string {
	return filepath.Join(filepath.Dir(launchPath(exe)), "tailreach-capture")
}

// managedCopy is true for an install that `tailreach setup` looks after (Homebrew's, or the
// copy it keeps), rather than a checkout built with make.
func managedCopy(exe string) bool {
	return launchPath(exe) != exe || filepath.Dir(exe) == installedDir()
}
