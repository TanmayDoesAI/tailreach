package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// grantPermissions walks someone through Screen Recording and Accessibility during
// `tailreach setup`, instead of leaving them with macOS's prompt and a list of red lines.
// It opens the right pane, says what to switch on, waits for Enter, restarts tailreach
// cleanly when Screen Recording needs it, and checks again. It never changes a setting
// itself, and only runs in a terminal: scripts and agents get doctor's report as before.
func grantPermissions(stateDir, exe string) {
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return
	}
	p := diagWhenReady(stateDir, true) // also puts tailreach in both lists
	in := bufio.NewReader(os.Stdin)
	for round := 0; p != nil; round++ {
		screen, _ := p["screen"].(bool)
		access, _ := p["accessibility"].(bool)
		if screen && access {
			if round > 0 {
				fmt.Println("✓ Screen Recording and Accessibility allowed")
			}
			return
		}
		if round == 3 {
			fmt.Println("Still not allowed. Leaving it for now: the check below says what's missing.")
			return
		}
		fmt.Println()
		fmt.Println("tailreach needs two macOS permissions. In System Settings → Privacy & Security:")
		if !screen {
			fmt.Println("  • Screen Recording: switch on tailreach (so your other device sees the screen)")
		}
		if !access {
			fmt.Println("  • Accessibility: switch on tailreach (so taps and typing reach the Mac)")
		}
		fmt.Printf("  Not in the list? Click +, press ⌘⇧G and open %s\n", filepath.Dir(launchPath(exe)))
		if !screen {
			fmt.Println("  If macOS offers \"Quit & Reopen\", choose Later: setup restarts tailreach properly.")
		}
		pane := "Privacy_ScreenCapture"
		if screen {
			pane = "Privacy_Accessibility"
		}
		exec.Command("open", "x-apple.systempreferences:com.apple.preference.security?"+pane).Run()
		fmt.Print("Press Enter once they're on (Ctrl-C to do it later) ")
		if _, err := in.ReadString('\n'); err != nil {
			fmt.Println()
			return
		}
		// Accessibility applies at once; Screen Recording only after the helper restarts.
		if !screen {
			fmt.Println("… restarting tailreach so the new permission takes effect")
			exec.Command("launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchLabel)).Run()
			time.Sleep(2 * time.Second)
		}
		p = diagWhenReady(stateDir, false)
	}
}

// diagWhenReady asks the capture helper what macOS allows, waiting up to 30 s for
// tailreach to come up. nil when it can't say (still signing in to Tailscale, someone
// viewing, an error): doctor reports those.
func diagWhenReady(stateDir string, ask bool) map[string]any {
	for wait := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		p, err := controlCall(stateDir, map[string]any{"op": "diag", "ask": ask})
		if err == nil && p["error"] == nil && p["busy"] == nil {
			return p
		}
		starting := err != nil || p["starting"] == true
		if !starting || signInLink(stateDir) != "" || time.Now().After(wait) {
			return nil
		}
	}
}
