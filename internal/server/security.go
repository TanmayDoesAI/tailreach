package server

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/argon2"
)

// Device password: every viewing device (phone, tablet, other computer) enters it
// once, then is trusted for 30 days. Trust is bound to that device's Tailscale node,
// so a copied cookie is useless anywhere else. Managed from the Mac's own settings
// window (`tailreach settings`), never over the network.

const (
	trustCookie   = "md_trust"
	trustTTL      = 30 * 24 * time.Hour
	maxPwFailures = 5
	pwLockout     = 15 * time.Minute
)

type trustEntry struct {
	ID      string    `json:"id"`   // sha256 of the cookie value
	Node    string    `json:"node"` // Tailscale StableNodeID it is bound to
	Device  string    `json:"device"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}

type secConfig struct {
	KeepAwake       bool   `json:"keepAwake"`                // stop the Mac sleeping (on power) so it stays reachable; on unless chosen off
	AwakeOnBattery  bool   `json:"awakeOnBattery,omitempty"` // ...and on battery too, down to batteryFloor
	Paused          bool   `json:"paused,omitempty"`         // sharing paused from the menu bar: no streams until resumed
	MenuBar         *bool  `json:"menuBar,omitempty"`        // nil = shown
	PasswordEnabled bool   `json:"passwordEnabled"`
	NoUpdateCheck   bool   `json:"noUpdateCheck,omitempty"` // don't ask GitHub for new releases
	Hash            []byte `json:"hash,omitempty"`
	Salt            []byte `json:"salt,omitempty"`
}

type security struct {
	mu        sync.Mutex
	awake     *exec.Cmd
	awakeMode string    // what awake holds: "" | "ac" | "all" (battery too)
	power     powerInfo // as of the last applyKeepAwake
	dir       string
	cfg       secConfig
	trust     []trustEntry
	failures  int
	lockedTil time.Time
}

func newSecurity(dir string) *security {
	s := &security{dir: dir}
	s.cfg.KeepAwake = true // a new install stays reachable; a choice already saved wins
	if b, err := os.ReadFile(filepath.Join(dir, "security.json")); err == nil {
		json.Unmarshal(b, &s.cfg)
	}
	sharingPaused.Store(s.cfg.Paused)
	if b, err := os.ReadFile(filepath.Join(dir, "trusted.json")); err == nil {
		json.Unmarshal(b, &s.trust)
	}
	return s
}

func (s *security) saveLocked() {
	b, _ := json.MarshalIndent(s.cfg, "", "  ")
	os.WriteFile(filepath.Join(s.dir, "security.json"), b, 0o600)
	b, _ = json.MarshalIndent(s.trust, "", "  ")
	os.WriteFile(filepath.Join(s.dir, "trusted.json"), b, 0o600)
}

func hashPassword(pw string, salt []byte) []byte {
	return argon2.IDKey([]byte(pw), salt, 3, 64*1024, 2, 32)
}

func (s *security) required() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.PasswordEnabled && len(s.cfg.Hash) > 0
}

// trusted reports whether this request's device entered the password in the last 30 days.
func (s *security) trusted(r *http.Request, node string) bool {
	c, err := r.Cookie(trustCookie)
	if err != nil || node == "" {
		return false
	}
	id := tokenHash(c.Value)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.trust {
		if t.ID == id && t.Node == node && now.Before(t.Expires) {
			return true
		}
	}
	return false
}

var errLocked = errors.New("too many wrong attempts, try again later")

func (s *security) checkPassword(pw string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().Before(s.lockedTil) {
		return errLocked
	}
	if subtle.ConstantTimeCompare(hashPassword(pw, s.cfg.Salt), s.cfg.Hash) == 1 {
		s.failures = 0
		return nil
	}
	s.failures++
	if s.failures >= maxPwFailures {
		s.failures = 0
		s.lockedTil = time.Now().Add(pwLockout)
		return errLocked
	}
	return errors.New("wrong password")
}

func (s *security) addTrust(w http.ResponseWriter, node, device string) {
	tok := base64.RawURLEncoding.EncodeToString(randBytes(32))
	now := time.Now()
	s.mu.Lock()
	kept := s.trust[:0]
	for _, t := range s.trust { // one entry per device, drop expired ones
		if t.Node != node && now.Before(t.Expires) {
			kept = append(kept, t)
		}
	}
	s.trust = append(kept, trustEntry{ID: tokenHash(tok), Node: node, Device: device, Created: now, Expires: now.Add(trustTTL)})
	s.saveLocked()
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: trustCookie, Value: tok, Path: "/", MaxAge: int(trustTTL.Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

// dropTrust forgets this browser's 30-day trust, so it needs the password again.
func (s *security) dropTrust(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(trustCookie); err == nil {
		id := tokenHash(c.Value)
		s.mu.Lock()
		kept := s.trust[:0]
		for _, t := range s.trust {
			if t.ID != id {
				kept = append(kept, t)
			}
		}
		s.trust = kept
		s.saveLocked()
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: trustCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
}

// ---------- HTTP: entering the password ----------

func (a *app) viewerNode(r *http.Request) string {
	if who, err := a.lc.WhoIs(r.Context(), r.RemoteAddr); err == nil && who.Node != nil {
		return string(who.Node.StableID)
	}
	return ""
}

// passwordOK is true when no password is required or this device is trusted.
func (a *app) passwordOK(r *http.Request) bool {
	return !a.sec.required() || a.sec.trusted(r, a.viewerNode(r))
}

func (a *app) passwordLogin(w http.ResponseWriter, r *http.Request) {
	if !a.sameOrigin(r) {
		http.Error(w, "bad origin", http.StatusForbidden)
		return
	}
	var body struct{ Password string }
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	who := a.viewerName(r)
	if err := a.sec.checkPassword(body.Password); err != nil {
		a.audit("password-failed", who, err.Error())
		if err == errLocked {
			notify("Password locked for 15 minutes", "too many wrong attempts from "+who)
		}
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	a.sec.addTrust(w, a.viewerNode(r), who)
	a.audit("password-ok", who, "trusted for 30 days")
	notify("Device trusted for 30 days", who)
	// The device password (on top of your Tailscale identity) is enough to sign in;
	// a passkey is an optional shortcut, never a lock-out.
	a.auth2.startSession(w, who)
	w.WriteHeader(http.StatusNoContent)
}

// ---------- live sessions (shown in settings, stoppable from the Mac) ----------

type liveSession struct {
	ID      int64     `json:"id"`
	Kind    string    `json:"kind"` // "stream" | "vnc"
	Viewer  string    `json:"viewer"`
	Guest   string    `json:"guest,omitempty"` // the guest's login, if it's a guest
	Started time.Time `json:"started"`
	cancel  context.CancelFunc
}

var live = struct {
	sync.Mutex
	next int64
	m    map[int64]*liveSession
}{m: map[int64]*liveSession{}}

// trackSession registers a session (guest = the guest's login, or ""); exclusive=true
// ends other sessions of the same kind.
func trackSession(kind, viewer, guest string, cancel context.CancelFunc, exclusive bool) func() {
	live.Lock()
	defer live.Unlock()
	if exclusive {
		for _, s := range live.m {
			if s.Kind == kind {
				s.cancel()
			}
		}
	}
	live.next++
	id := live.next
	live.m[id] = &liveSession{ID: id, Kind: kind, Viewer: viewer, Guest: guest, Started: time.Now(), cancel: cancel}
	return func() {
		live.Lock()
		delete(live.m, id)
		live.Unlock()
	}
}

// endGuest signs a guest out and ends whatever they have open, at once: their invite was
// removed or has ended.
func (a *app) endGuest(login string) {
	if a.auth2 != nil {
		a.auth2.dropGuestSessions(login)
	}
	live.Lock()
	defer live.Unlock()
	for _, s := range live.m {
		if s.Guest != "" && loginKey(s.Guest) == loginKey(login) {
			s.cancel()
		}
	}
}

// expireInvites ends invites as they run out (watchAuth also re-checks open streams).
func (a *app) expireInvites() {
	for {
		for _, inv := range a.invites.prune() {
			log.Printf("■ Invite for %s ended", inv.Login)
			a.audit("invite-expired", inv.Login, "")
			a.endGuest(inv.Login)
		}
		time.Sleep(30 * time.Second)
	}
}

func stopAllSessions() {
	releaseVNC()
	live.Lock()
	defer live.Unlock()
	for _, s := range live.m {
		s.cancel()
	}
}

// ---------- control socket for the local settings window ----------
//
// A Unix socket in the state dir (mode 0600). Every connection is additionally
// checked to come from the same macOS user. Not reachable from the network at all.

func ControlSocketPath(stateDir string) string { return filepath.Join(stateDir, "control.sock") }

// The socket is up from the start, before Tailscale has signed in (the menu bar shows
// that and the sign-in link); ctlApp is set once tailreach is ready for everything else.
var (
	ctlApp    atomic.Pointer[app]
	signInURL atomic.Pointer[string] // the Tailscale sign-in link a first run waits on
)

func serveControl(path string) {
	ln, err := listenControl(path)
	if err != nil {
		log.Printf("settings socket: %v", err)
		return
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		if !sameUser(c) {
			c.Close()
			continue
		}
		go handleControl(c)
	}
}

func handleControl(c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	enc := json.NewEncoder(c)
	for sc.Scan() {
		var req map[string]any
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			return
		}
		if a := ctlApp.Load(); a != nil {
			enc.Encode(a.control(req))
			continue
		}
		st := map[string]any{"starting": true, "version": Version, "error": "tailreach is still signing in to Tailscale."}
		if u := signInURL.Load(); u != nil {
			st["authURL"] = *u
		}
		enc.Encode(st)
	}
}

func (a *app) control(req map[string]any) map[string]any {
	op, _ := req["op"].(string)
	s := a.sec
	if (op == "setPassword" || op == "enablePassword") && a.auth2 == nil {
		// Behind a hub, signing in happens on the hub, once for every Mac behind it.
		return map[string]any{"error": "This Mac is reached through your hub (" + a.opt.via + "), so the password is set there: run `tailreach password` (or `tailreach settings`) on the hub. Signing in on the hub opens every Mac behind it."}
	}
	switch op {
	case "status":
		// handled below
	case "setPassword":
		pw, _ := req["password"].(string)
		if len([]rune(pw)) < 8 {
			return map[string]any{"error": "Use at least 8 characters."}
		}
		salt := randBytes(16)
		s.mu.Lock()
		s.cfg.PasswordEnabled, s.cfg.Salt, s.cfg.Hash = true, salt, hashPassword(pw, salt)
		s.trust = nil // everyone re-enters the new password
		s.saveLocked()
		s.mu.Unlock()
		a.audit("password-changed", "this Mac", "")
		stopAllSessions() // open streams re-check sign-in when they reconnect
	case "enablePassword":
		on, _ := req["enabled"].(bool)
		s.mu.Lock()
		if on && len(s.cfg.Hash) == 0 {
			s.mu.Unlock()
			return map[string]any{"error": "Set a password first."}
		}
		s.cfg.PasswordEnabled = on
		s.saveLocked()
		s.mu.Unlock()
		a.audit("password-enabled", "this Mac", map[bool]string{true: "on", false: "off"}[on])
		if on {
			stopAllSessions()
		}
	case "keepAwake", "awakeOnBattery":
		on, _ := req["enabled"].(bool)
		s.mu.Lock()
		if op == "keepAwake" {
			s.cfg.KeepAwake = on
		} else {
			s.cfg.AwakeOnBattery = on
		}
		s.saveLocked()
		s.mu.Unlock()
		s.applyKeepAwake()
	case "updateCheck": // the once-a-day "is there a newer tailreach?" question to GitHub
		on, _ := req["enabled"].(bool)
		s.mu.Lock()
		s.cfg.NoUpdateCheck = !on
		s.saveLocked()
		s.mu.Unlock()
		if on && a.updates != nil {
			select {
			case a.updates.kick <- struct{}{}:
			default:
			}
		}
	case "pause": // the menu bar's Pause sharing: no streams at all until resumed, even after a restart
		on, _ := req["paused"].(bool)
		s.mu.Lock()
		s.cfg.Paused = on
		s.saveLocked()
		s.mu.Unlock()
		sharingPaused.Store(on)
		a.audit("sharing-paused", "this Mac", map[bool]string{true: "on", false: "off"}[on])
		if on {
			log.Printf("■ sharing paused on the Mac")
			endStreams()
		} else {
			log.Printf("▶ sharing resumed")
		}
	case "stopStream": // the menu bar's Stop: the same as Stop on the panel
		log.Printf("■ stopped from the Mac")
		a.audit("stopped-on-mac", "this Mac", "")
		blockStreams(stopHold)
		endStreams()
	case "forgetDevice": // `tailreach forget NAME`: a reinstalled Mac behind this hub can pair again
		name, _ := req["name"].(string)
		if !a.forgetDevice(sanitizeName(name)) {
			return map[string]any{"error": "no Mac called " + name + " is known to this hub"}
		}
		a.audit("device-forgotten", "this Mac", name)
	case "menuBar":
		on, _ := req["enabled"].(bool)
		s.mu.Lock()
		s.cfg.MenuBar = &on
		s.saveLocked()
		s.mu.Unlock()
		setMenuBar(on)
	case "revokeTrust":
		id, _ := req["id"].(string)
		s.mu.Lock()
		kept := s.trust[:0]
		for _, t := range s.trust {
			if id != "*" && t.ID != id {
				kept = append(kept, t)
			}
		}
		s.trust = kept
		s.saveLocked()
		s.mu.Unlock()
		a.audit("trust-revoked", "this Mac", id)
		stopAllSessions() // open streams re-check sign-in when they reconnect
	case "resetPasskeys":
		if a.auth2 != nil {
			a.auth2.mu.Lock()
			a.auth2.user.Creds = nil
			a.auth2.sessions = map[string]*session{}
			a.auth2.save()
			a.auth2.mu.Unlock()
		}
		a.audit("passkeys-reset", "this Mac", "")
		stopAllSessions() // open streams re-check sign-in when they reconnect
	case "testSession":
		// For local testing tools (tools/streamtest): a short session cookie. Only reachable
		// through this owner-only socket, i.e. by someone already logged in to this Mac.
		if a.auth2 == nil {
			return map[string]any{"error": "no sign-in on this device"}
		}
		tok := base64.RawURLEncoding.EncodeToString(randBytes(32))
		now := time.Now()
		a.auth2.mu.Lock()
		a.auth2.sessions[tokenHash(tok)] = &session{created: now, seen: now, expires: now.Add(15 * time.Minute), device: "test tool"}
		a.auth2.mu.Unlock()
		cookies := sessionCookie + "=" + tok
		// If a device password is on, also trust the tool's tailnet IP for 15 minutes.
		if ip, _ := req["ip"].(string); ip != "" && s.required() {
			if who, err := a.lc.WhoIs(context.Background(), net.JoinHostPort(ip, "1")); err == nil && who.Node != nil {
				tt := base64.RawURLEncoding.EncodeToString(randBytes(32))
				s.mu.Lock()
				s.trust = append(s.trust, trustEntry{ID: tokenHash(tt), Node: string(who.Node.StableID), Device: "test tool", Created: now, Expires: now.Add(15 * time.Minute)})
				s.saveLocked()
				s.mu.Unlock()
				cookies += "; " + trustCookie + "=" + tt
			}
		}
		a.audit("test-session", "this Mac", "")
		return map[string]any{"cookie": cookies}
	case "recordViewer":
		secs, _ := req["secs"].(float64)
		secs = min(max(secs, 0), 1800)
		path := a.startViewerRec(int(secs))
		a.audit("viewer-stats", "this Mac", fmt.Sprintf("%ds", int(secs)))
		return map[string]any{"path": path, "secs": int(secs)}
	case "diag": // for `tailreach doctor`: what macOS allows the capture helper
		if !a.fastAvailable() {
			return map[string]any{"error": "no capture helper next to tailreach"}
		}
		ask, _ := req["ask"].(bool)
		res, busy, err := helperDiag(ask)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		if busy {
			return map[string]any{"busy": true}
		}
		if ask { // files sent from the viewer go to Downloads, which macOS guards too
			res["downloads"] = downloadsWritable()
		}
		return res
	case "invite": // `tailreach invite` and the settings window: let a guest in for a while
		if a.invites == nil {
			return map[string]any{"error": "Guests are invited on the hub (or a Mac running on its own), not on a Mac behind a hub."}
		}
		login, _ := req["login"].(string)
		hours, _ := req["hours"].(float64)
		viewOnly, _ := req["viewOnly"].(bool)
		if hours < 0 || hours > inviteMax.Hours() {
			return map[string]any{"error": "An invite lasts 1 to 720 hours (30 days)."}
		}
		if l := loginKey(login); l == loginKey(a.owner) {
			return map[string]any{"error": "That's this Mac's own Tailscale account: it already has access."}
		} else if a.allow[strings.TrimSpace(login)] {
			return map[string]any{"error": "That account already has full access (--allow)."}
		}
		code, inv, err := a.invites.add(login, time.Duration(hours*float64(time.Hour)), viewOnly)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		a.endGuest(inv.Login) // a new invite starts afresh: earlier sessions and the old code end
		mode := "can control"
		if inv.ViewOnly {
			mode = "view only"
		}
		a.audit("invite-created", inv.Login, mode+", until "+inv.Expires.Format(time.RFC3339))
		log.Printf("✓ Invited %s until %s (%s)", inv.Login, inv.Expires.Format("Jan 2 15:04"), mode)
		st := a.control(map[string]any{"op": "status"})
		host, _, _ := strings.Cut(a.selfDNS, ".")
		addr := "https://" + a.selfDNS + "/"
		if a.opt.hub {
			addr += a.opt.name + "/"
		}
		st["invited"] = map[string]any{"login": inv.Login, "code": code, "expires": inv.Expires, "viewOnly": inv.ViewOnly,
			"address": addr, "machine": host, "admin": "https://login.tailscale.com/admin/machines"}
		return st
	case "uninvite":
		login, _ := req["login"].(string)
		if a.invites == nil || !a.invites.remove(login) {
			return map[string]any{"error": "No invite for " + login + "."}
		}
		a.audit("invite-removed", login, "")
		log.Printf("■ Invite for %s removed", login)
		a.endGuest(login)
	case "stopSessions":
		stopAllSessions()
		if a.auth2 != nil {
			a.auth2.mu.Lock()
			a.auth2.sessions = map[string]*session{}
			a.auth2.mu.Unlock()
		}
		a.audit("sessions-stopped", "this Mac", "")
	default:
		return map[string]any{"error": "unknown op"}
	}

	// Every reply carries the current state.
	s.mu.Lock()
	trust := []trustEntry{}
	for _, t := range s.trust {
		if time.Now().Before(t.Expires) {
			trust = append(trust, t)
		}
	}
	st := map[string]any{
		"passwordEnabled": s.cfg.PasswordEnabled,
		"passwordSet":     len(s.cfg.Hash) > 0,
		"trusted":         trust,
		"paused":          s.cfg.Paused,
		"menuBar":         s.cfg.MenuBar == nil || *s.cfg.MenuBar,
	}
	s.awakeStatus(st)
	s.mu.Unlock()
	passkeys := 0
	if a.auth2 != nil {
		a.auth2.mu.Lock()
		passkeys = len(a.auth2.user.Creds)
		a.auth2.mu.Unlock()
	}
	live.Lock()
	sessions := []*liveSession{}
	for _, l := range live.m {
		sessions = append(sessions, l)
	}
	live.Unlock()
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	st["passkeys"] = passkeys
	st["sessions"] = sessions
	st["url"] = "https://" + a.selfDNS + "/"
	st["owner"] = a.owner
	st["managesLogin"] = a.auth2 != nil
	st["mode"] = a.mode()
	if a.auth2 == nil {
		st["hub"] = a.opt.via
	}
	st["https"] = a.https
	st["screenSharing"] = a.vncUp()
	st["name"] = a.opt.name
	st["version"] = Version
	st["debug"] = debugLog
	st["viewerURL"], st["dashboardURL"] = a.links()
	if brief, _ := req["brief"].(bool); !brief { // the menu bar asks often and doesn't need it
		st["ownDevices"] = a.ownDevices()
	}
	st["updateCheck"] = a.updateCheckOn()
	st["updateCheckFlagOff"] = a.opt.noUpdateCheck
	if up := a.updateNotice(); up != nil {
		st["update"] = up
	}
	st["invites"] = []invite{}
	if a.invites != nil {
		st["invites"] = a.invites.active()
	}
	return st
}

// mode is how this Mac is reached: "hub" (it serves the dashboard and the sign-in for
// every Mac behind it), "behindHub" (only through its hub, which signs viewers in), or
// "standalone" (on its own, with its own sign-in).
func (a *app) mode() string {
	switch {
	case a.opt.hub:
		return "hub"
	case a.auth2 == nil:
		return "behindHub"
	}
	return "standalone"
}

// links are this Mac's viewer and the dashboard, as the owner opens them.
func (a *app) links() (viewer, dashboard string) {
	switch {
	case a.opt.hub:
		dashboard = "https://" + a.selfDNS + "/"
		return dashboard + a.opt.name + "/", dashboard
	case a.opt.via != "": // behind a hub: reached through it
		dashboard = "https://" + a.opt.via + "." + a.domain + "/"
		return dashboard + a.opt.name + "/", dashboard
	}
	dashboard = "https://" + a.selfDNS + "/"
	return dashboard, dashboard
}

// ownDevices counts the owner's other devices on the tailnet (a phone, a laptop: what
// they'll view from), or -1 if Tailscale didn't say.
func (a *app) ownDevices() int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := a.lc.Status(ctx)
	if err != nil {
		return -1
	}
	n := 0
	for _, p := range s.Peer {
		if u, ok := s.User[p.UserID]; ok && u.LoginName == a.owner && (p.Tags == nil || p.Tags.Len() == 0) {
			n++
		}
	}
	return n
}
