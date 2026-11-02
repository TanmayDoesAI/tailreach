package server

import (
	"bufio"
	"encoding/json"
	"net"
	"testing"
)

// Before Tailscale has signed in, the control socket already answers: that it's starting,
// with the sign-in link, and nothing else.
func TestControlWhileSigningIn(t *testing.T) {
	ctlApp.Store(nil)
	url := "https://login.tailscale.com/a/example"
	signInURL.Store(&url)
	defer signInURL.Store(nil)

	c, s := net.Pipe()
	go handleControl(s)
	defer c.Close()
	rd := bufio.NewReader(c)
	for _, op := range []string{"status", "pause"} {
		json.NewEncoder(c).Encode(map[string]any{"op": op, "paused": true})
		line, err := rd.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var st map[string]any
		json.Unmarshal(line, &st)
		if st["starting"] != true || st["authURL"] != url || st["error"] == nil {
			t.Fatalf("%s while signing in: %v", op, st)
		}
	}
	if sharingPaused.Load() {
		t.Fatal("pause went through before tailreach was ready")
	}
}
