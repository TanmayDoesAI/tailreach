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
	"strconv"
	"strings"
	"time"
)

// doctor checks a setup end to end and says, for anything wrong, exactly what to do.
// It changes nothing, except that --ask lets macOS list tailreach in both Privacy panes.
// Exit status 1 means something needs fixing, so scripts and coding agents can rely on it.
func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	state := fs.String("state", defaultStateDir(), "state directory of the running tailreach")
	ask := fs.Bool("ask", false, "put tailreach in the Screen Recording and Accessibility lists (macOS may show its prompt)")
	fs.Parse(args)

	d := &checkup{}
	fmt.Printf("tailreach doctor (%s)\n\n", version)
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	mac := runtime.GOOS == "darwin"
	// The fixes name the commands for this kind of install: a Homebrew copy or a checkout.
	me, restart, rebuild := "./tailreach", "`make reload`", "`make local && make reload`"
	if launchPath(exe) != exe {
		me, restart, rebuild = "tailreach", "`tailreach setup`", "`tailreach setup`"
	}

	if mac {
		d.section("This Mac")
		if out, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
			v := strings.TrimSpace(string(out))
			if major, _, _ := strings.Cut(v, "."); atoi(major) < 14 {
				d.fail("macOS "+v+": tailreach needs macOS 14 (Sonoma) or newer", "update macOS in System Settings → General → Software Update")
			}
		}
		if out, _ := exec.Command("sysctl", "-n", "hw.optional.arm64").Output(); strings.TrimSpace(string(out)) == "1" && runtime.GOARCH == "amd64" {
			d.warn("Built for Intel, so it runs through Rosetta and uses more CPU while streaming",
				"`make reload` (it builds for this Mac's CPU), or `tailreach setup` for a Homebrew copy")
		} else {
			d.ok("Native build (%s)", runtime.GOARCH)
		}
		if exec.Command("security", "find-certificate", "-c", "tailreach local").Run() == nil {
			d.ok("Signing identity \"tailreach local\"")
		} else {
			d.fail("No signing identity \"tailreach local\": permissions would reset on every build", setupFix(exe, "sh scripts/signing-identity.sh, then `make reload`"))
		}
		for _, b := range []struct{ path, id string }{{exe, "com.tailreach.agent"}, {helperPath(), "com.tailreach.capture"}} {
			name := filepath.Base(b.path)
			out, err := exec.Command("codesign", "-dv", "--verbose=2", b.path).CombinedOutput()
			switch {
			case err != nil && strings.Contains(string(out), "No such file"):
				d.fail(name+" is missing", setupFix(exe, "`make local`")+" (Homebrew: `brew reinstall tailreach`)")
			case !strings.Contains(string(out), "Authority=tailreach local") || !strings.Contains(string(out), "Identifier="+b.id):
				d.fail(name+" isn't signed with \"tailreach local\"", setupFix(exe, "make local (after sh scripts/signing-identity.sh)"))
			default:
				d.ok("%s signed", name)
			}
		}
		d.loginItem(exe, me, restart)
	}

	d.section("tailreach")
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
		log := filepath.Join(*state, "tailreach.log")
		switch url := signInLink(*state); {
		case url != "":
			d.fail("Waiting for Tailscale sign-in", "open "+url+" and approve it. No Tailscale account? That page makes one, free.\n"+
				"        Then install the Tailscale app on your phone and sign in with the same account.")
		case lastError(log) != "":
			d.fail("tailreach stopped with an error: "+lastError(log), "fix that, then "+restart+". The whole log: "+log)
		default:
			d.fail("tailreach isn't running", setupFix(exe, "`./tailreach install --hub --name mac` (or `make setup`)")+"; its log: "+log)
		}
		return d.done()
	}
	if v, _ := st["version"].(string); v != "" && v != version {
		d.warn(fmt.Sprintf("The running tailreach is %s, this one is %s", v, version), restart)
	}
	if b, _ := st["debug"].(bool); b {
		d.warn("Debug logging is on (a DEBUG=1 build)", "`make reload` for a normal build")
	}
	d.ok("Running, signed in to Tailscale as %v", st["owner"])
	if b, _ := st["https"].(bool); b {
		d.ok("HTTPS certificates on")
	} else {
		d.fail("HTTPS certificates are off for this tailnet: browsers can't connect", "turn on MagicDNS and HTTPS Certificates at https://login.tailscale.com/admin/dns, then "+restart)
	}
	switch n, ok := st["ownDevices"].(float64); {
	case !ok: // an older tailreach that doesn't say
	case n == 0:
		d.fail("No other device of yours is on this tailnet: nothing can view this Mac yet",
			"install the Tailscale app on your phone, tablet or laptop and sign in with "+fmt.Sprint(st["owner"]))
	case n > 0:
		d.ok("%d other device(s) of yours on the tailnet", int(n))
	}
	if b, _ := st["managesLogin"].(bool); b {
		if set, _ := st["passwordSet"].(bool); set {
			d.ok("Device password set")
		} else {
			d.warn("No device password: new devices sign in with a 6-digit code shown on this Mac", "run `"+me+" password` yourself (not an agent: it's your password)")
		}
	}
	if b, _ := st["screenSharing"].(bool); b {
		d.ok("Screen Sharing on (unlocking the lock screen)")
	} else if mac {
		d.warn("Screen Sharing is off: you can't unlock the Mac from the phone", "System Settings → General → Sharing → Screen Sharing")
	}
	// Guests aren't a problem, just worth knowing about: who else can get in, and until when.
	if inv, _ := st["invites"].([]any); len(inv) > 0 {
		first, _ := inv[0].(map[string]any) // soonest to end first
		end, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(first["expires"]))
		fmt.Printf("  – %d guest invite(s) running, the first ends %s (`%s invite --list`, `--remove LOGIN`)\n",
			len(inv), end.Local().Format("Mon Jan 2 15:04"), me)
	}

	if mac {
		d.section("Permissions")
		p, err := controlCall(*state, map[string]any{"op": "diag", "ask": *ask})
		switch {
		case err != nil:
			d.fail("Couldn't ask the capture helper: "+err.Error(), restart+", then try again")
		case p["error"] != nil:
			d.fail(fmt.Sprint(p["error"]), rebuild)
		case p["busy"] == true:
			fmt.Println("  – Not checked: someone is viewing right now. Run it again when nobody is.")
		default:
			fix := "System Settings → Privacy & Security → %s: switch on tailreach, then " + restart + ". Not listed? `" + me + " doctor --ask`"
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
				d.warn("Can't save to Downloads: files sent from the viewer won't arrive", "System Settings → Privacy & Security → Files & Folders → tailreach: Downloads Folder")
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

// setupFix is the fix for a Homebrew copy (`tailreach setup`), or else the one for a checkout.
func setupFix(exe, checkout string) string {
	if launchPath(exe) != exe {
		return "tailreach setup"
	}
	return checkout
}

// loginItem checks that the LaunchAgent exists, runs this very binary and is running.
func (d *checkup) loginItem(exe, me, restart string) {
	out, err := exec.Command("plutil", "-extract", "ProgramArguments.0", "raw", "-o", "-", plistPath()).Output()
	if err != nil {
		d.fail("Doesn't start at login", setupFix(exe, "`./tailreach install --hub --name mac` (or `make setup`)"))
		return
	}
	if got := strings.TrimSpace(string(out)); got != launchPath(exe) {
		d.warn("The login item runs "+got+", not this copy", setupFix(exe, "run `./tailreach install …` from the copy you want, with the same flags"))
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
		d.fail("The login item isn't running", restart+"; if it keeps stopping, `"+me+" doctor` shows the error from its log")
	}
}

// signInLink finds the Tailscale sign-in link a first run is waiting on, in the log.
func signInLink(stateDir string) string {
	b, err := os.ReadFile(filepath.Join(stateDir, "tailreach.log"))
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

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// lastError is the error a failed start left at the end of the log ("tailreach: …"),
// unless it started fine after that.
func lastError(logPath string) string {
	b, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-100; i-- {
		if strings.Contains(lines[i], "✓ Signed in as") {
			return ""
		}
		if e, ok := strings.CutPrefix(lines[i], "tailreach: "); ok {
			return e
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

// stuckHelp is the way out when a fix doesn't work: a coding agent with the repo's own
// setup guide, or an issue.
func stuckHelp() {
	fmt.Println(`
Stuck? Let a coding agent (Claude Code, Codex, Cursor…) finish it with you:
  git clone https://github.com/TanmayDoesAI/tailreach && cd tailreach
  then ask it: "Set up tailreach on this Mac by following SETUP.md. Stop and tell me
  whenever I need to do something myself."
Or open an issue, with this output: https://github.com/TanmayDoesAI/tailreach/issues`)
}

func (d *checkup) done() error {
	fmt.Println()
	switch {
	case d.failed > 0:
		fmt.Printf("%d to fix, %d to consider.\n", d.failed, d.warned)
		stuckHelp()
		os.Exit(1)
	case d.warned > 0:
		fmt.Printf("Working. %d to consider.\n", d.warned)
	default:
		fmt.Println("All good.")
	}
	return nil
}
