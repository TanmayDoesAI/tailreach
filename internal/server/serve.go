// Package server is tailreach itself: it joins the tailnet (tsnet), serves the dashboard,
// sign-in and viewer, and relays the screen and input between the browser and the macOS
// capture helper.
package server

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/TanmayDoesAI/tailreach/web"
	"tailscale.com/client/local"
	"tailscale.com/tsnet"
)

// Version is this build's version.
const Version = "0.2.0"

type options struct {
	name          string
	hub           bool
	hubName       string
	vncAddr       string
	stateDir      string
	webDir        string
	allow         string
	via           string
	verbose       bool
	noUpdateCheck bool
}

// Serve runs tailreach with the given command-line flags until it fails.
func Serve(args []string) error {
	log.SetFlags(log.Ltime)
	mime.AddExtensionType(".webmanifest", "application/manifest+json")
	fs := flag.NewFlagSet("tailreach", flag.ExitOnError)
	var o options
	fs.StringVar(&o.name, "name", defaultName(), "name of this device in links, e.g. mac → /mac/")
	fs.BoolVar(&o.hub, "hub", false, "also serve the dashboard of all devices")
	fs.StringVar(&o.hubName, "hub-name", "devices", "tailnet hostname for the hub: https://<hub-name>.<tailnet>.ts.net, or just type <hub-name>/ in a browser on your tailnet")
	fs.StringVar(&o.vncAddr, "vnc", "127.0.0.1:5900", "local VNC server to expose")
	fs.StringVar(&o.stateDir, "state", DefaultStateDir(), "where Tailscale state is kept")
	fs.StringVar(&o.webDir, "web-dir", "", "serve the web UI from this folder instead of the built-in copy (for hacking on the UI)")
	fs.StringVar(&o.allow, "allow", "", "extra Tailscale logins allowed in, comma-separated (default: only the owner)")
	fs.StringVar(&o.via, "via", "devices", "devices: only accept connections through this hub (\"\" = standalone, with its own Face ID login)")
	fs.BoolVar(&o.verbose, "v", false, "verbose Tailscale logs")
	fs.BoolVar(&o.noUpdateCheck, "no-update-check", false, "never ask GitHub whether a newer tailreach is out (the only request besides Tailscale's)")
	fs.Usage = usage
	fs.Parse(args)

	o.name = sanitizeName(o.name)
	if o.name == "" {
		return fmt.Errorf("--name must contain letters or digits")
	}
	return run(o)
}

func run(o options) error {
	MigrateState()
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

	// Up before signing in to Tailscale: keep-awake, and the control socket and menu bar,
	// which show that sign-in is pending (and its link) until it's done.
	sec := newSecurity(filepath.Join(o.stateDir, stateName))
	sec.applyKeepAwake()
	if runtime.GOOS == "darwin" {
		go sec.keepAwakeLoop()
	}
	sock := ControlSocketPath(o.stateDir)
	go serveControl(sock)
	sec.mu.Lock()
	showMenuBar := sec.cfg.MenuBar == nil || *sec.cfg.MenuBar
	sec.mu.Unlock()
	startMenuBar(sock, showMenuBar)

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
	a.sec = sec
	if runtime.GOOS == "darwin" && os.Getppid() != 1 {
		log.Printf("ℹ Running from a terminal: during sessions macOS will show that terminal app as sharing the screen. For everyday use run `tailreach install` instead.")
	}
	a.updates = newUpdateChecker(Version)
	go a.updateLoop(ctx)
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
		a.web = web.FS
	}
	ctlApp.Store(a) // the control socket answers everything from now on

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
		a.loadPins()
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
				signInURL.Store(&shown)
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
`, Version)
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

func DefaultStateDir() string {
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

func firstIPv4(ips []netip.Addr) string {
	for _, ip := range ips {
		if ip.Is4() {
			return ip.String()
		}
	}
	return ""
}
