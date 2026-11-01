// tailreach — reach your own computers' screens from any browser on your tailnet.
//
// Every machine runs this binary. It joins your tailnet as its own device (via
// tsnet, so no separate Tailscale setup is needed) and relays the browser's
// WebSocket to the local VNC server (macOS Screen Sharing by default).
//
// One machine runs with --hub: it joins as "devices" and serves a dashboard of every
// device at https://devices.<tailnet>.ts.net/ (or just devices/), each at /<name>/.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"
	"tailscale.com/client/local"
	"tailscale.com/tsnet"
)

const version = "0.2.0"

//go:embed all:web
var embedded embed.FS

type options struct {
	name     string
	hub      bool
	hubName  string
	vncAddr  string
	stateDir string
	webDir   string
	allow    string
	via      string
	verbose  bool
}

func main() {
	log.SetFlags(log.Ltime)
	mime.AddExtensionType(".webmanifest", "application/manifest+json")

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			if err := setup(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "tailreach:", err)
				stuckHelp()
				os.Exit(1)
			}
			return
		case "install":
			exitOn(install(os.Args[2:]))
			return
		case "uninstall":
			exitOn(uninstall())
			return
		case "password":
			exitOn(passwordCmd(os.Args[2:]))
			return
		case "invite":
			exitOn(inviteCmd(os.Args[2:]))
			return
		case "settings":
			exitOn(openSettings(os.Args[2:]))
			return
		case "doctor":
			exitOn(doctor(os.Args[2:]))
			return
		case "version":
			if debugLog {
				fmt.Println(version, "(debug build)")
			} else {
				fmt.Println(version)
			}
			return
		case "vdisplay": // internal: owns the extended display (see vdisplay.go)
			os.Exit(vdisplayChild(os.Args[2:]))
		}
	}

	var o options
	flag.StringVar(&o.name, "name", defaultName(), "name of this device in links, e.g. mac → /mac/")
	flag.BoolVar(&o.hub, "hub", false, "also serve the dashboard of all devices")
	flag.StringVar(&o.hubName, "hub-name", "devices", "tailnet hostname for the hub: https://<hub-name>.<tailnet>.ts.net, or just type <hub-name>/ in a browser on your tailnet")
	flag.StringVar(&o.vncAddr, "vnc", "127.0.0.1:5900", "local VNC server to expose")
	flag.StringVar(&o.stateDir, "state", defaultStateDir(), "where Tailscale state is kept")
	flag.StringVar(&o.webDir, "web-dir", "", "serve the web UI from this folder instead of the built-in copy (for hacking on the UI)")
	flag.StringVar(&o.allow, "allow", "", "extra Tailscale logins allowed in, comma-separated (default: only the owner)")
	flag.StringVar(&o.via, "via", "devices", "devices: only accept connections through this hub (\"\" = standalone, with its own Face ID login)")
	flag.BoolVar(&o.verbose, "v", false, "verbose Tailscale logs")
	flag.Usage = usage
	flag.Parse()

	o.name = sanitizeName(o.name)
	if o.name == "" {
		exitOn(fmt.Errorf("--name must contain letters or digits"))
	}
	exitOn(run(o))
}

func run(o options) error {
	migrateState()
	retireHelperOnExit()
	hostname := o.name
	stateName := o.name
	if o.hub {
		hostname = sanitizeName(o.hubName)
		stateName = "tailreach" // not the hub's name, so renaming the hub keeps its Tailscale login
		o.via = ""
	}
	o.via = sanitizeName(o.via)

	srv := &tsnet.Server{
		Hostname: hostname,
		Dir:      filepath.Join(o.stateDir, stateName),
		AuthKey:  os.Getenv("TS_AUTHKEY"),
		Logf:     func(string, ...any) {},
		UserLogf: func(format string, args ...any) {}, // the login URL is printed by watchLogin instead
	}
	if o.verbose {
		srv.Logf = log.Printf
	}
	defer srv.Close()

	if err := srv.Start(); err != nil {
		return err
	}
	lc, err := srv.LocalClient()
	if err != nil {
		return err
	}

	ctx := context.Background()
	go watchLogin(ctx, lc)
	st, err := srv.Up(ctx)
	if err != nil {
		return err
	}

	a := &app{
		opt:     o,
		srv:     srv,
		lc:      lc,
		domain:  st.CurrentTailnet.MagicDNSSuffix,
		selfDNS: strings.TrimSuffix(st.Self.DNSName, "."),
		tsIP4:   firstIPv4(st.Self.TailscaleIPs),
		allow:   map[string]bool{},
		devices: map[string]*device{},
	}
	// Up's status can come back without the user table; ask again until we know the owner.
	for i := 0; i < 50 && a.owner == ""; i++ {
		if full, err := lc.Status(ctx); err == nil {
			if u, ok := full.User[full.Self.UserID]; ok {
				a.owner = u.LoginName
			}
		}
		if a.owner == "" {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if a.owner == "" {
		return fmt.Errorf("couldn't determine which Tailscale user owns this device")
	}
	for _, l := range strings.Split(o.allow, ",") {
		if l = strings.TrimSpace(l); l != "" {
			a.allow[l] = true
		}
	}
	a.sec = newSecurity(filepath.Join(o.stateDir, stateName))
	if runtime.GOOS == "darwin" && os.Getppid() != 1 {
		log.Printf("ℹ Running from a terminal: during sessions macOS will show that terminal app as sharing the screen. For everyday use run `tailreach install` instead.")
	}
	a.sec.applyKeepAwake()
	go a.serveControl()
	if o.hub || o.via == "" {
		if a.auth2, err = newAuth(a.selfDNS, filepath.Join(o.stateDir, stateName), a.owner); err != nil {
			return err
		}
		a.invites = newInviteStore(filepath.Join(o.stateDir, stateName))
		go a.expireInvites()
	}
	if o.webDir != "" {
		a.web = os.DirFS(o.webDir)
	} else {
		a.web, _ = fs.Sub(embedded, "web")
	}

	a.https = len(st.CertDomains) > 0
	if !a.https {
		log.Printf("⚠ HTTPS certificates are off for this tailnet. Turn on MagicDNS and HTTPS at")
		log.Printf("  https://login.tailscale.com/admin/dns — the remote screen needs https to log in.")
	}
	if !a.vncUp() {
		log.Printf("⚠ Nothing is listening on %s.", o.vncAddr)
		if runtime.GOOS == "darwin" {
			log.Printf("  Turn on System Settings → General → Sharing → Screen Sharing.")
		}
	}

	ln80, err := srv.Listen("tcp", ":80")
	if err != nil {
		return err
	}
	ln443, err := srv.ListenTLS("tcp", ":443")
	if err != nil {
		return err
	}

	if o.hub {
		a.loadKnown()
		go a.discoverLoop(ctx)
	}

	base := "https://" + a.selfDNS
	log.Printf("✓ Signed in as %s on %s", a.owner, st.CurrentTailnet.Name)
	dlog("debug build: debug logging is on (lines marked ·)")
	if o.hub {
		log.Printf("✓ Dashboard:  %s/   (or type %s/ in a browser on your tailnet)", base, hostname)
		log.Printf("✓ This Mac:   %s/%s/", base, o.name)
	} else {
		log.Printf("✓ This device: %s/", base)
	}

	go func() { log.Fatal(http.Serve(ln80, a.plainHandler())) }()
	return (&http.Server{Handler: a.handler(), ReadHeaderTimeout: 10 * time.Second}).Serve(ln443)
}

// watchLogin prints (and opens) the Tailscale login link the first time a device runs.
func watchLogin(ctx context.Context, lc *local.Client) {
	shown := ""
	for {
		st, err := lc.StatusWithoutPeers(ctx)
		if err == nil {
			if st.BackendState == "Running" {
				return
			}
			if st.AuthURL != "" && st.AuthURL != shown {
				shown = st.AuthURL
				log.Printf("→ Sign in to Tailscale to add this device: %s", st.AuthURL)
				openBrowser(st.AuthURL)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `tailreach %s — your screens, from any browser on your tailnet

Usage:
  tailreach [flags]              run on this machine (prints a login link the first time)
  tailreach --hub [flags]        also serve the dashboard at https://devices.<tailnet>.ts.net/
  tailreach setup [flags]        set up an installed copy (Homebrew): sign it, login item, doctor
  tailreach install [flags]      start automatically at login (macOS), same flags as above
  tailreach uninstall            stop and remove the login item
  tailreach settings             open the settings window on this Mac (password, trusted devices, sessions)
  tailreach password             set or reset the device password from a terminal (--off turns it off)
  tailreach invite LOGIN         let someone else in for a while (--hours N, --view-only; --list, --remove LOGIN)
  tailreach doctor               check the setup and say how to fix what isn't right

Flags:
`, version)
	flag.PrintDefaults()
}

var nameRe = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitizeName(s string) string {
	s = strings.ToLower(strings.TrimSuffix(s, ".local"))
	s = nameRe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

func defaultName() string {
	h, _ := os.Hostname()
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return sanitizeName(h)
}

func defaultStateDir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "tailreach")
}

func openBrowser(url string) {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", url).Start()
	case "linux":
		exec.Command("xdg-open", url).Start()
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func exitOn(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "tailreach:", err)
		os.Exit(1)
	}
}

// openSettings opens the native settings window, talking to the running tailreach over its local socket.
func openSettings(args []string) error {
	fs := flag.NewFlagSet("settings", flag.ExitOnError)
	state := fs.String("state", defaultStateDir(), "state directory of the running tailreach")
	fs.Parse(args)
	sock := controlSocketPath(*state)
	if _, err := os.Stat(sock); err != nil {
		return fmt.Errorf("tailreach isn't running (no %s)", sock)
	}
	cmd := exec.Command(helperPath(), "--settings", sock)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// passwordCmd sets, resets or turns off the device password via the running tailreach.
func passwordCmd(args []string) error {
	fs := flag.NewFlagSet("password", flag.ExitOnError)
	state := fs.String("state", defaultStateDir(), "state directory of the running tailreach")
	off := fs.Bool("off", false, "turn the password off")
	fs.Parse(args)
	req := map[string]any{"op": "enablePassword", "enabled": false}
	if !*off {
		fmt.Print("New device password (8+ characters): ")
		p1, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return err
		}
		fmt.Print("Again: ")
		p2, _ := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if string(p1) != string(p2) {
			return fmt.Errorf("passwords don't match")
		}
		req = map[string]any{"op": "setPassword", "password": string(p1)}
	}
	c, err := net.Dial("unix", controlSocketPath(*state))
	if err != nil {
		return fmt.Errorf("tailreach isn't running: %v", err)
	}
	defer c.Close()
	json.NewEncoder(c).Encode(req)
	var reply map[string]any
	if err := json.NewDecoder(c).Decode(&reply); err != nil {
		return err
	}
	if e, ok := reply["error"].(string); ok {
		return errors.New(e)
	}
	if *off {
		fmt.Println("✓ Password turned off")
	} else {
		fmt.Println("✓ Password set. Every device will be asked for it again (then trusted for 30 days).")
	}
	return nil
}

func firstIPv4(ips []netip.Addr) string {
	for _, ip := range ips {
		if ip.Is4() {
			return ip.String()
		}
	}
	return ""
}

// inviteCmd lets someone else in for a while, from this Mac only (the running tailreach,
// over its control socket): `tailreach invite name@example.com [--hours N] [--view-only]`,
// `--list`, `--remove name@example.com`.
func inviteCmd(args []string) error {
	fs := flag.NewFlagSet("invite", flag.ExitOnError)
	state := fs.String("state", defaultStateDir(), "state directory of the running tailreach")
	hours := fs.Int("hours", 24, "how long the invite lasts, in hours (up to 720 = 30 days)")
	viewOnly := fs.Bool("view-only", false, "they can watch the screen, not control it")
	list := fs.Bool("list", false, "list running invites")
	remove := fs.String("remove", "", "end this login's invite now (and anything they have open)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: tailreach invite THEIR-TAILSCALE-LOGIN [--hours N] [--view-only]\n       tailreach invite --list\n       tailreach invite --remove THEIR-TAILSCALE-LOGIN")
		fs.PrintDefaults()
	}
	// Flags may come before or after the login.
	var pos []string
	for {
		fs.Parse(args)
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	call := func(req map[string]any) (map[string]any, error) {
		st, err := controlCall(*state, req)
		if err != nil {
			return nil, fmt.Errorf("tailreach isn't running: %v", err)
		}
		if e, ok := st["error"].(string); ok {
			return nil, errors.New(e)
		}
		return st, nil
	}
	switch {
	case *remove != "":
		if _, err := call(map[string]any{"op": "uninvite", "login": *remove}); err != nil {
			return err
		}
		fmt.Printf("✓ %s can't get in any more; anything they had open was ended.\n", *remove)
		return nil
	case *list:
		st, err := call(map[string]any{"op": "status"})
		if err != nil {
			return err
		}
		inv, _ := st["invites"].([]any)
		if len(inv) == 0 {
			fmt.Println("No guests invited.")
			return nil
		}
		for _, x := range inv {
			m, _ := x.(map[string]any)
			exp, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(m["expires"]))
			mode := "can control"
			if m["viewOnly"] == true {
				mode = "view only"
			}
			code := "code not used yet"
			if _, used := m["used"]; used {
				code = "signed in"
			}
			fmt.Printf("  %s  %s, until %s (%s)\n", m["login"], mode, exp.Local().Format("Mon Jan 2 15:04"), code)
		}
		return nil
	case len(pos) != 1:
		fs.Usage()
		os.Exit(2)
	}
	if *hours < 1 || *hours > 720 {
		return fmt.Errorf("--hours is 1 to 720 (30 days)")
	}
	st, err := call(map[string]any{"op": "invite", "login": pos[0], "hours": *hours, "viewOnly": *viewOnly})
	if err != nil {
		return err
	}
	in, _ := st["invited"].(map[string]any)
	exp, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(in["expires"]))
	mode := "can see and control this Mac"
	if *viewOnly {
		mode = "can watch this Mac's screen, not control it"
	}
	fmt.Printf(`✓ Invited %s until %s: %s.

What they need:
  1. You share this Mac with them in Tailscale (free, works across Tailscale accounts):
     %s → the machine "%s" → ⋯ → Share… → send them the link it makes.
  2. They accept it, signed in to Tailscale as %s, then open
     %s
  3. The sign-in page asks them for this invite code:  %s
     (one use, and it ends with the invite. Run this again for a new code.)

This Mac shows who's connected while they view. End it any time:
  tailreach invite --remove %s
`, in["login"], exp.Local().Format("Mon Jan 2 15:04"), mode, in["admin"], in["machine"], in["login"], in["address"], in["code"], in["login"])
	return nil
}
