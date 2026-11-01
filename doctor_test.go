package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLogScans(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "mydevices.log")
	for _, c := range []struct{ log, err, link string }{
		{"10:00:00 → Sign in to Tailscale to add this device: https://login.tailscale.com/a/x\n", "", "https://login.tailscale.com/a/x"},
		{"10:00:00 ✓ Signed in as a on b\n", "", ""},
		{"10:00:00 ✓ Signed in as a on b\nmydevices: couldn't determine which Tailscale user owns this device\n", "couldn't determine which Tailscale user owns this device", ""},
		{"mydevices: old failure\n10:00:00 ✓ Signed in as a on b\n", "", ""},
	} {
		os.WriteFile(log, []byte(c.log), 0o600)
		if got := lastError(log); got != c.err {
			t.Errorf("lastError(%q) = %q, want %q", c.log, got, c.err)
		}
		if got := signInLink(dir); got != c.link {
			t.Errorf("signInLink(%q) = %q, want %q", c.log, got, c.link)
		}
	}
}
