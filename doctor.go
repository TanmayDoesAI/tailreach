package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// doctor checks a setup end to end and says, for anything wrong, exactly what to do.
// It changes nothing, except that --ask lets macOS list mydevices in both Privacy panes.
// Exit status 1 means something needs fixing, so scripts and coding agents can rely on it.
func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	state := fs.String("state", defaultStateDir(), "state directory of the running mydevices")
	ask := fs.Bool("ask", false, "put mydevices in the Screen Recording and Accessibility lists (macOS may show its prompt)")
	fs.Parse(args)

	d := &checkup{}
	fmt.Printf("tailreach doctor (mydevices %s)\n\n", version)
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	mac := runtime.GOOS == "darwin"

	if mac {
		d.section("This Mac")
		if out, _ := exec.Command("sysctl", "-n", "hw.optional.arm64").Output(); strings.TrimSpace(string(out)) == "1" && runtime.GOARCH == "amd64" {
			d.warn("Built for Intel, so it runs through Rosetta and uses more CPU while streaming",
				"install the Apple silicon Go (go.dev/dl, darwin-arm64), then `make reload`")
		} else {
			d.ok("Native build (%s)", runtime.GOARCH)
		}
		if exec.Command("security", "find-certificate", "-c", "mydevices local").Run() == nil {
			d.ok("Signing identity \"mydevices local\"")
		} else {
			d.fail("No signing identity \"mydevices local\": permissions would reset on every build", "sh scripts/signing-identity.sh, then `make reload`")
		}
		for _, b := range []struct{ path, id string }{{exe, "com.mydevices.agent"}, {helperPath(), "com.mydevices.capture"}} {
			name := filepath.Base(b.path)
			out, err := exec.Command("codesign", "-dv", "--verbose=2", b.path).CombinedOutput()
			switch {
			case err != nil && strings.Contains(string(out), "No such file"):
				d.fail(name+" is missing", "make local")
			case !strings.Contains(string(out), "Authority=mydevices local") || !strings.Contains(string(out), "Identifier="+b.id):
				d.fail(name+" isn't signed with \"mydevices local\"", "make local (after sh scripts/signing-identity.sh)")
			default:
				d.ok("%s signed", name)
			}
		}
		d.loginItem(exe)
	}

	d.section("mydevices")
	st, err := controlCall(*state, map[string]any{"op": "status"})
	for wait := time.Now().Add(20 * time.Second); err != nil && d.running && signInLink(*state) == "" && time.Now().Before(wait); {
		if !d.said {
			fmt.Println("  … starting up (joining Tailscale), waiting a few seconds")
			d.said = true
		}
		time.Sleep(time.Second)
		st, err = controlCall(*state, map[string]any{"op": "status"})
	}
	if err != nil {
		if url := signInLink(*state); url != "" {
			d.fail("Waiting for Tailscale sign-in", "open "+url+" and approve it with your Tailscale account")
		} else {
			d.fail("mydevices isn't running", "`./mydevices install --hub --name mac` (or `make setup`); its log: "+filepath.Join(*state, "mydevices.log"))
		}
		return d.done()
	}
	if v, _ := st["version"].(string); v != "" && v != version {
		d.warn(fmt.Sprintf("The running mydevices is %s, this one is %s", v, version), "make reload")
	}
	if b, _ := st["debug"].(bool); b {
		d.warn("Debug logging is on (a DEBUG=1 build)", "`make reload` for a normal build")
	}
	d.ok("Running, signed in to Tailscale as %v", st["owner"])
	if b, _ := st["https"].(bool); b {
		d.ok("HTTPS certificates on")
	} else {
		d.fail("HTTPS certificates are off for this tailnet: browsers can't connect", "turn on MagicDNS and HTTPS at https://login.tailscale.com/admin/dns, then `make reload`")
	}
	if b, _ := st["managesLogin"].(bool); b {
		if set, _ := st["passwordSet"].(bool); set {
			d.ok("Device password set")
		} else {
			d.warn("No device password: new devices sign in with a 6-digit code shown on this Mac", "run `./mydevices password` yourself (not an agent: it's your password)")
		}
	}
	if b, _ := st["screenSharing"].(bool); b {
		d.ok("Screen Sharing on (unlocking the lock screen)")
	} else if mac {
		d.warn("Screen Sharing is off: you can't unlock the Mac from the phone", "System Settings → General → Sharing → Screen Sharing")
	}

	if mac {
		d.section("Permissions")
		p, err := controlCall(*state, map[string]any{"op": "diag", "ask": *ask})
		switch {
		case err != nil:
			d.fail("Couldn't ask the capture helper: "+err.Error(), "make reload, then try again")
		case p["error"] != nil:
			d.fail(fmt.Sprint(p["error"]), "make local && make reload")
		case p["busy"] == true:
			fmt.Println("  – Not checked: someone is viewing right now. Run it again when nobody is.")
		default:
			fix := "System Settings → Privacy & Security → %s: switch on mydevices, then `make reload`. Not listed? `./mydevices doctor --ask`"
			if b, _ := p["screen"].(bool); b {
				d.ok("Screen Recording")
			} else {
				d.fail("Screen Recording not allowed: the phone gets a black screen", fmt.Sprintf(fix, "Screen Recording"))
			}
			if b, _ := p["accessibility"].(bool); b {
				d.ok("Accessibility")
			} else {
				d.fail("Accessibility not allowed: taps and typing won't reach the Mac", fmt.Sprintf(fix, "Accessibility"))
			}
			if b, ok := p["downloads"].(bool); ok && b {
				d.ok("Downloads (files sent from the viewer)")
			} else if ok {
				d.warn("Can't save to Downloads: files sent from the viewer won't arrive", "System Settings → Privacy & Security → Files & Folders → mydevices: Downloads Folder")
			}
		}
	}

	if url, _ := st["url"].(string); url != "" {
		d.section("Use it")
		name, _ := st["name"].(string)
		fmt.Printf("  On your phone (Tailscale app connected): %s%s/\n", url, name)
	}
	return d.done()
}

// loginItem checks that the LaunchAgent exists, runs this very binary and is running.
func (d *checkup) loginItem(exe string) {
	out, err := exec.Command("plutil", "-extract", "ProgramArguments.0", "raw", "-o", "-", plistPath()).Output()
	if err != nil {
		d.fail("Doesn't start at login", "`./mydevices install --hub --name mac` (or `make setup`)")
		return
	}
	if got := strings.TrimSpace(string(out)); got != exe {
		d.warn("The login item runs "+got+", not this copy", "run `./mydevices install …` from the copy you want, with the same flags")
	}
	// Just after a rebuild, launchd waits a few seconds before starting it again.
	running := func() bool {
		pr, _ := exec.Command("launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchLabel)).Output()
		return regexp.MustCompile(`(?m)^\s*state = running`).Match(pr)
	}
	up := running()
	for wait := time.Now().Add(15 * time.Second); !up && time.Now().Before(wait); up = running() {
		time.Sleep(time.Second)
	}
	if up {
		d.running = true
		d.ok("Starts at login, running")
	} else {
		d.fail("The login item isn't running", "make reload; if it keeps stopping, read the end of the log")
	}
}

// signInLink finds the Tailscale sign-in link a first run is waiting on, in the log.
func signInLink(stateDir string) string {
	b, err := os.ReadFile(filepath.Join(stateDir, "mydevices.log"))
	if err != nil {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-200; i-- {
		if strings.Contains(lines[i], "✓ Signed in as") {
			return ""
		}
		if _, url, ok := strings.Cut(lines[i], "Sign in to Tailscale to add this device: "); ok {
			return strings.TrimSpace(url)
		}
	}
	return ""
}

func controlCall(stateDir string, req map[string]any) (map[string]any, error) {
	c, err := net.DialTimeout("unix", controlSocketPath(stateDir), 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return nil, err
	}
	var reply map[string]any
	return reply, json.NewDecoder(c).Decode(&reply)
}

type checkup struct {
	failed, warned int
	running        bool // the login item is up (it may still be joining Tailscale)
	said           bool
}

func (d *checkup) section(name string) { fmt.Printf("%s\n", name) }
func (d *checkup) ok(format string, a ...any) {
	fmt.Printf("  ✓ %s\n", fmt.Sprintf(format, a...))
}
func (d *checkup) warn(what, fix string) {
	d.warned++
	fmt.Printf("  ! %s\n      → %s\n", what, fix)
}
func (d *checkup) fail(what, fix string) {
	d.failed++
	fmt.Printf("  ✗ %s\n      → %s\n", what, fix)
}

func (d *checkup) done() error {
	fmt.Println()
	switch {
	case d.failed > 0:
		fmt.Printf("%d to fix, %d to consider.\n", d.failed, d.warned)
		os.Exit(1)
	case d.warned > 0:
		fmt.Printf("Working. %d to consider.\n", d.warned)
	default:
		fmt.Println("All good.")
	}
	return nil
}
