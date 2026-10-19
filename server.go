package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"tailscale.com/client/local"
	"tailscale.com/tsnet"
)

type app struct {
	opt     options
	srv     *tsnet.Server
	lc      *local.Client
	web     fs.FS
	domain  string // MagicDNS suffix, e.g. tail1234.ts.net
	selfDNS string // this node, e.g. mydevices.tail1234.ts.net
	owner   string // Tailscale login of whoever signed this device in
	allow   map[string]bool
	auth2   *authState // passkeys (hub / standalone)
	sec     *security  // device password + trusted devices

	hubMu sync.Mutex
	hubID string // pinned StableID of the hub (devices behind a hub)

	mu      sync.RWMutex
	devices map[string]*device // hub only, keyed by device name

	vncMu    sync.Mutex
	vncAt    time.Time
	vncState bool
}

// device is one machine running mydevices, as seen by the hub.
type device struct {
	Name     string    `json:"name"`
	Host     string    `json:"host"` // tailnet hostname
	OS       string    `json:"os"`
	Online   bool      `json:"online"` // reachable on the tailnet and mydevices is running
	Screen   bool      `json:"screen"` // its VNC / Screen Sharing is on
	Local    bool      `json:"local"`  // this is the hub machine itself
	LastSeen time.Time `json:"lastSeen"`
	IP       string    `json:"-"`
}

// info is what every node reports at /.well-known/mydevices.json.
type info struct {
	Name    string `json:"name"`
	OS      string `json:"os"`
	Screen  bool   `json:"screen"`
	Version string `json:"version"`
}

// ---------- routing ----------

func (a *app) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/mydevices.json", a.serveInfo)
	if a.auth2 != nil {
		a.authRoutes(mux)
	}

	viewer := a.viewer()
	if a.opt.hub {
		mux.HandleFunc("GET /{$}", a.serveFile("dashboard/index.html"))
		mux.HandleFunc("GET /api/devices", a.serveDevices)
		mux.HandleFunc("/{name}/", func(w http.ResponseWriter, r *http.Request) {
			name := r.PathValue("name")
			if name == a.opt.name {
				http.StripPrefix("/"+name, viewer).ServeHTTP(w, r)
				return
			}
			a.proxyToDevice(w, r, name)
		})
		mux.HandleFunc("GET /{name}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/"+r.PathValue("name")+"/", http.StatusFound)
		})
	} else {
		mux.Handle("/", viewer)
		mux.Handle("/"+a.opt.name+"/", http.StripPrefix("/"+a.opt.name, viewer))
	}
	return a.auth(a.gate(mux))
}

// gate is the second layer after Tailscale identity:
// the hub (and standalone devices) require a passkey session; devices behind a hub
// only talk to that hub.
func (a *app) gate(next http.Handler) http.Handler {
	if a.auth2 != nil {
		return a.requireSession(next)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.fromHub(r) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			hub := "https://" + a.opt.via + "." + a.domain + "/" + a.opt.name + "/"
			fmt.Fprintf(w, `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1"><body style="font:16px system-ui;background:#0b0d12;color:#e9ecf2;padding:40px">This device is reached through your hub: <a style="color:#4f8cff" href="%s">%s</a>`, hub, html.EscapeString(hub))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// fromHub reports whether the request comes from this device's hub. The first time
// the hub connects, its stable node ID is pinned; after that only that exact node counts.
func (a *app) fromHub(r *http.Request) bool {
	if a.opt.via == "" {
		return false
	}
	who, err := a.lc.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil || who.Node == nil {
		return false
	}
	host, _, _ := strings.Cut(who.Node.Name, ".")
	if host != a.opt.via {
		return false
	}
	id := string(who.Node.StableID)
	a.hubMu.Lock()
	defer a.hubMu.Unlock()
	pin := filepath.Join(a.opt.stateDir, a.opt.name, "hub-id")
	if a.hubID == "" {
		if b, err := os.ReadFile(pin); err == nil {
			a.hubID = strings.TrimSpace(string(b))
		}
	}
	if a.hubID == "" {
		a.hubID = id
		os.WriteFile(pin, []byte(id), 0o600)
		log.Printf("✓ Paired with hub %s", who.Node.Name)
	}
	if a.hubID != id {
		log.Printf("✗ %s claims to be the hub but isn't the one this device paired with (delete %s to re-pair)", who.Node.Name, pin)
		return false
	}
	return true
}

// plainHandler serves port 80: the hub talks to devices over it (the tailnet
// already encrypts it); browsers get redirected to https.
func (a *app) plainHandler() http.Handler {
	h := a.handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Mydevices-Hub") != "" || strings.HasPrefix(r.URL.Path, "/.well-known/") {
			h.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, "https://"+a.selfDNS+r.URL.RequestURI(), http.StatusFound)
	})
}

// viewer serves the remote-screen page and its WebSocket.
func (a *app) viewer() http.Handler {
	sub, _ := fs.Sub(a.web, "viewer")
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/websockify":
			a.relayVNC(w, r)
			return
		case "/stream":
			a.relayStream(w, r)
			return
		case "/caps":
			if a.fastAvailable() {
				go warmHelper() // the viewer is about to connect: get capture ready
			}
			writeJSON(w, map[string]any{"name": a.opt.name, "fast": a.fastAvailable(), "vnc": a.vncUp(), "user": consoleUser()})
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

func (a *app) serveFile(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(a.web, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	}
}

// ---------- security ----------

// auth lets in only the Tailscale user who signed this device in (plus --allow).
// Tailscale tells us who is on the other end of every connection, so there is
// no password to leak; the Mac's own login is still required for the screen.
func (a *app) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who, err := a.lc.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil || who.Node == nil || who.UserProfile == nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		login := who.UserProfile.LoginName
		if who.Node.IsTagged() || (login != a.owner && !a.allow[login]) {
			log.Printf("✗ denied %s from %s (%s)", r.URL.Path, login, who.Node.ComputedName)
			http.Error(w, "forbidden: "+login+" is not allowed on this device", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' wss:; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// ---------- the screen relay ----------

func (a *app) relayVNC(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{"binary"},
		// Only pages served from this tailnet may open the socket.
		OriginPatterns:  []string{"*." + a.domain},
		CompressionMode: websocket.CompressionDisabled, // VNC data is already compressed
	})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(16 << 20)

	tcp, err := net.DialTimeout("tcp", a.opt.vncAddr, 5*time.Second)
	if err != nil {
		ws.Close(websocket.StatusTryAgainLater, "screen sharing is off on this device")
		return
	}
	defer tcp.Close()

	who := a.viewerName(r)
	log.Printf("▶ VNC session started for %s", who)
	a.audit("vnc-start", who, "")
	notify("Screen is being viewed (VNC)", "from "+who)
	start := time.Now()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer trackSession("vnc", who, cancel, false)()
	go func() { <-ctx.Done(); tcp.Close() }()
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	go func() {
		io.Copy(conn, tcp)
		cancel()
	}()
	io.Copy(tcp, conn)
	log.Printf("■ VNC session ended after %s", time.Since(start).Round(time.Second))
	a.audit("vnc-end", who, time.Since(start).Round(time.Second).String())
}

func (a *app) vncUp() bool {
	a.vncMu.Lock()
	defer a.vncMu.Unlock()
	if time.Since(a.vncAt) < 5*time.Second {
		return a.vncState
	}
	c, err := net.DialTimeout("tcp", a.opt.vncAddr, 500*time.Millisecond)
	if err == nil {
		c.Close()
	}
	a.vncState, a.vncAt = err == nil, time.Now()
	return a.vncState
}

func (a *app) serveInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, info{Name: a.opt.name, OS: runtime.GOOS, Screen: a.vncUp(), Version: version})
}

// ---------- hub: finding devices ----------

func (a *app) serveDevices(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	list := []*device{{Name: a.opt.name, Host: a.selfDNS, OS: runtime.GOOS, Online: true, Screen: a.vncUp(), Local: true, LastSeen: time.Now()}}
	for _, d := range a.devices {
		if d.Name != a.opt.name {
			c := *d
			list = append(list, &c)
		}
	}
	a.mu.RUnlock()
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Local != list[j].Local {
			return list[i].Local
		}
		if list[i].Online != list[j].Online {
			return list[i].Online
		}
		return list[i].Name < list[j].Name
	})
	writeJSON(w, map[string]any{"devices": list, "owner": a.owner, "hub": a.selfDNS})
}

func (a *app) discoverLoop(ctx context.Context) {
	for {
		a.discover(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

// discover asks every online peer owned by the same person whether it runs mydevices.
func (a *app) discover(ctx context.Context) {
	st, err := a.lc.Status(ctx)
	if err != nil {
		return
	}
	client := a.srv.HTTPClient()
	client.Timeout = 3 * time.Second

	seen := map[string]bool{}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, p := range st.Peer {
		if p.UserID != st.Self.UserID || !p.Online || len(p.TailscaleIPs) == 0 {
			continue
		}
		ip := p.TailscaleIPs[0].String()
		host := strings.TrimSuffix(p.DNSName, ".")
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+ip+"/.well-known/mydevices.json", nil)
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			var in info
			if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&in) != nil {
				return
			}
			name := sanitizeName(in.Name)
			if name == "" {
				return
			}
			mu.Lock()
			seen[name] = true
			mu.Unlock()
			a.mu.Lock()
			a.devices[name] = &device{Name: name, Host: host, OS: in.OS, Online: true, Screen: in.Screen, LastSeen: time.Now(), IP: ip}
			a.mu.Unlock()
		}()
	}
	wg.Wait()

	a.mu.Lock()
	for name, d := range a.devices {
		if !seen[name] {
			d.Online, d.Screen = false, false
		}
	}
	a.mu.Unlock()
	a.saveKnown()
}

// Known devices are remembered so the dashboard can list them while they're offline.
func (a *app) knownPath() string {
	return filepath.Join(a.opt.stateDir, "devices.json")
}

func (a *app) loadKnown() {
	b, err := os.ReadFile(a.knownPath())
	if err != nil {
		return
	}
	var list []*device
	if json.Unmarshal(b, &list) == nil {
		for _, d := range list {
			d.Online, d.Screen = false, false
			a.devices[d.Name] = d
		}
	}
}

func (a *app) saveKnown() {
	a.mu.RLock()
	list := make([]*device, 0, len(a.devices))
	for _, d := range a.devices {
		list = append(list, d)
	}
	a.mu.RUnlock()
	b, _ := json.MarshalIndent(list, "", "  ")
	os.WriteFile(a.knownPath(), b, 0o600)
}

// ---------- hub: forwarding /<name>/ to that device ----------

func (a *app) proxyToDevice(w http.ResponseWriter, r *http.Request, name string) {
	viewer := a.viewerName(r)
	a.mu.RLock()
	d := a.devices[name]
	var ip string
	online := false
	if d != nil {
		ip, online = d.IP, d.Online
	}
	a.mu.RUnlock()

	if d == nil || !online || ip == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		state := "isn't known to this hub"
		if d != nil {
			state = "is offline"
		}
		fmt.Fprintf(w, `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1"><body style="font:16px system-ui;background:#0b0d12;color:#e9ecf2;padding:40px"><h2>%s %s</h2><p><a style="color:#4f8cff" href="/">← All devices</a></p>`, html.EscapeString(name), state)
		return
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: ip})
			pr.Out.Host = ip
			pr.Out.Header.Set("X-Mydevices-Hub", "1")
			pr.Out.Header.Set("X-Mydevices-Viewer", viewer)
		},
		Transport:     a.srv.HTTPClient().Transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, name+" didn't answer: "+err.Error(), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// consoleUser is the macOS account name, used to unlock the lock screen over Screen Sharing.
func consoleUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}
