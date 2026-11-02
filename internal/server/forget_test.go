package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestForgetDevice(t *testing.T) {
	a := &app{opt: options{stateDir: t.TempDir()}, devices: map[string]*device{"mac2": {Name: "mac2"}}}
	a.loadPins()
	a.nodePins["mac2"] = "nOLD"
	a.pinWarned["mac2 nNEW"] = true
	if !a.forgetDevice("mac2") {
		t.Fatal("forgetting a known device failed")
	}
	if _, ok := a.nodePins["mac2"]; ok || a.devices["mac2"] != nil || a.pinWarned["mac2 nNEW"] {
		t.Fatal("mac2 is still remembered")
	}
	if b, _ := os.ReadFile(filepath.Join(a.opt.stateDir, "device-nodes.json")); string(b) != "{}" {
		t.Fatalf("pins file = %q", b)
	}
	if a.forgetDevice("mac2") || a.forgetDevice("nope") {
		t.Fatal("forgetting an unknown device should report false")
	}
	// The next machine to answer as mac2 is taken as it.
	if !a.pinNode("mac2", "nNEW", "mac2.example.ts.net", map[string]bool{"nOLD": true}) {
		t.Fatal("a new machine wasn't accepted after forget")
	}
}
