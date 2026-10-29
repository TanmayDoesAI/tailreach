package main

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
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// Device password: every viewing device (phone, tablet, other computer) enters it
// once, then is trusted for 30 days. Trust is bound to that device's Tailscale node,
// so a copied cookie is useless anywhere else. Managed from the Mac's own settings
// window (`mydevices settings`), never over the network.

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
	KeepAwake       bool   `json:"keepAwake"` // stop the Mac sleeping (on power) so it stays reachable
	PasswordEnabled bool   `json:"passwordEnabled"`
	Hash            []byte `json:"hash,omitempty"`
	Salt            []byte `json:"salt,omitempty"`
}

type security struct {
	mu        sync.Mutex
	awake     *exec.Cmd
	dir       string
	cfg       secConfig
	trust     []trustEntry
	failures  int
	lockedTil time.Time
}

func newSecurity(dir string) *security {
	s := &security{dir: dir}
	if b, err := os.ReadFile(filepath.Join(dir, "security.json")); err == nil {
		json.Unmarshal(b, &s.cfg)
	}
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

// applyKeepAwake holds a "no system sleep" assertion while on AC power, so the Mac
// stays on the tailnet with the display off. Lid-closed / battery sleep still happens.
func (s *security) applyKeepAwake() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.KeepAwake && s.awake == nil && runtime.GOOS == "darwin" {
		c := exec.Command("caffeinate", "-s", "-w", strconv.Itoa(os.Getpid()))
		if c.Start() == nil {
			s.awake = c
		}
	} else if !s.cfg.KeepAwake && s.awake != nil {
		s.awake.Process.Kill()
		s.awake.Wait()
		s.awake = nil
	}
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
	Started time.Time `json:"started"`
	cancel  context.CancelFunc
}

var live = struct {
	sync.Mutex
	next int64
	m    map[int64]*liveSession
}{m: map[int64]*liveSession{}}

// trackSession registers a session; exclusive=true ends other sessions of the same kind.
func trackSession(kind, viewer string, cancel context.CancelFunc, exclusive bool) func() {
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
	live.m[id] = &liveSession{ID: id, Kind: kind, Viewer: viewer, Started: time.Now(), cancel: cancel}
	return func() {
		live.Lock()
		delete(live.m, id)
		live.Unlock()
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

func controlSocketPath(stateDir string) string { return filepath.Join(stateDir, "control.sock") }

func (a *app) serveControl() {
	path := controlSocketPath(a.opt.stateDir)
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
		go a.handleControl(c)
	}
}

func (a *app) handleControl(c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	enc := json.NewEncoder(c)
	for sc.Scan() {
		var req map[string]any
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			return
		}
		enc.Encode(a.control(req))
	}
}

func (a *app) control(req map[string]any) map[string]any {
	op, _ := req["op"].(string)
	s := a.sec
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
	case "keepAwake":
		on, _ := req["enabled"].(bool)
		s.mu.Lock()
		s.cfg.KeepAwake = on
		s.saveLocked()
		s.mu.Unlock()
		s.applyKeepAwake()
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
		a.auth2.sessions[tokenHash(tok)] = &session{created: now.Add(15*time.Minute - sessionMaxAge), seen: now, device: "test tool"}
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
	case "diag": // for `mydevices doctor`: what macOS allows the capture helper
		if !a.fastAvailable() {
			return map[string]any{"error": "no capture helper next to mydevices"}
		}
		ask, _ := req["ask"].(bool)
		res, busy, err := helperDiag(ask)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		if busy {
			return map[string]any{"busy": true}
		}
		return res
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
		"keepAwake":       s.cfg.KeepAwake,
		"trusted":         trust,
	}
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
	st["https"] = a.https
	st["screenSharing"] = a.vncUp()
	st["name"] = a.opt.name
	st["version"] = version
	st["debug"] = debugLog
	return st
}
