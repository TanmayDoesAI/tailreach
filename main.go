// mydevices — reach your own computers' screens from any browser on your tailnet.
//
// Every machine runs this binary. It joins your tailnet as its own device (via
// tsnet, so no separate Tailscale setup is needed) and relays the browser's
// WebSocket to the local VNC server (macOS Screen Sharing by default).
//
// One machine runs with --hub: it joins as "mydevices" and serves a dashboard of
// every device at https://mydevices.<tailnet>.ts.net/, with each device at /<name>/.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/tsnet"
)

const version = "0.1.0"

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
	verbose  bool
}

func main() {
	log.SetFlags(log.Ltime)

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "install":
			exitOn(install(os.Args[2:]))
			return
		case "uninstall":
			exitOn(uninstall())
			return
		case "version":
			fmt.Println(version)
			return
		}
	}

	var o options
	flag.StringVar(&o.name, "name", defaultName(), "name of this device in links, e.g. mac → /mac/")
	flag.BoolVar(&o.hub, "hub", false, "also serve the dashboard of all devices")
	flag.StringVar(&o.hubName, "hub-name", "mydevices", "tailnet hostname for the hub")
	flag.StringVar(&o.vncAddr, "vnc", "127.0.0.1:5900", "local VNC server to expose")
	flag.StringVar(&o.stateDir, "state", defaultStateDir(), "where Tailscale state is kept")
	flag.StringVar(&o.webDir, "web-dir", "", "serve the web UI from this folder instead of the built-in copy (for hacking on the UI)")
	flag.StringVar(&o.allow, "allow", "", "extra Tailscale logins allowed in, comma-separated (default: only the owner)")
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
	hostname := o.name
	if o.hub {
		hostname = sanitizeName(o.hubName)
	}

	srv := &tsnet.Server{
		Hostname: hostname,
		Dir:      filepath.Join(o.stateDir, hostname),
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
		allow:   map[string]bool{},
		devices: map[string]*device{},
	}
	if u, ok := st.User[st.Self.UserID]; ok {
		a.owner = u.LoginName
	}
	for _, l := range strings.Split(o.allow, ",") {
		if l = strings.TrimSpace(l); l != "" {
			a.allow[l] = true
		}
	}
	if o.webDir != "" {
		a.web = os.DirFS(o.webDir)
	} else {
		a.web, _ = fs.Sub(embedded, "web")
	}

	if len(st.CertDomains) == 0 {
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
	if o.hub {
		log.Printf("✓ Dashboard:  %s/", base)
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
	fmt.Fprintf(os.Stderr, `mydevices %s — your screens, from any browser on your tailnet

Usage:
  mydevices [flags]              run on this machine (prints a login link the first time)
  mydevices --hub [flags]        also serve the dashboard at https://mydevices.<tailnet>.ts.net/
  mydevices install [flags]      start automatically at login (macOS), same flags as above
  mydevices uninstall            stop and remove the login item

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
	return filepath.Join(d, "mydevices")
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
		fmt.Fprintln(os.Stderr, "mydevices:", err)
		os.Exit(1)
	}
}
