package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchPath(t *testing.T) {
	brew := "/opt/homebrew/Cellar/tailreach/0.2.2/libexec/tailreach"
	if got, want := launchPath(brew), filepath.Join(installedDir(), "tailreach"); got != want {
		t.Errorf("Homebrew copy: launchPath = %s, want %s", got, want)
	}
	if got := launchPath("/Users/me/src/tailreach/tailreach"); got != "/Users/me/src/tailreach/tailreach" {
		t.Errorf("checkout: launchPath = %s, want it unchanged", got)
	}
	for exe, want := range map[string]bool{brew: true, filepath.Join(installedDir(), "tailreach"): true, "/Users/me/src/tailreach/tailreach": false} {
		if managedCopy(exe) != want {
			t.Errorf("managedCopy(%s) = %v, want %v", exe, !want, want)
		}
	}
}

func TestCarryFlags(t *testing.T) {
	for _, c := range []struct{ in, want []string }{
		{[]string{"--hub", "--name", "mac", "--web-dir", "/Users/me/src/tailreach/web"}, []string{"--hub", "--name", "mac"}},
		{[]string{"--web-dir=/x", "-v", "--name", "mac2", "--via", ""}, []string{"--name", "mac2", "--via", ""}},
		{[]string{"--hub", "--no-update-check"}, []string{"--hub", "--no-update-check"}},
	} {
		if got := carryFlags(c.in); strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("carryFlags(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
