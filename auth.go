package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// Second factor on top of Tailscale identity: a passkey (Face ID / Touch ID).
//
//   - The first time a phone opens the hub it asks for a 6-digit pairing code, which
//     only appears on the Mac (notification + log). Then it registers a passkey.
//   - After that, opening a session is one Face ID prompt.
//   - Sessions are HttpOnly, SameSite=Strict cookies that expire after 12 h, or 1 h idle.
//
// Devices behind a hub skip this: they accept connections only from that hub.

const (
	sessionCookie = "md_session"
	sessionMaxAge = 12 * time.Hour
	sessionIdle   = 1 * time.Hour
	pairTTL       = 5 * time.Minute
	pairTries     = 5
)

type passkeyUser struct {
	RPID  string                `json:"rpID"` // passkeys only work on the address they were made for
	ID    []byte                `json:"id"`
	Name  string                `json:"name"`
	Creds []webauthn.Credential `json:"creds"`
}

func (u *passkeyUser) WebAuthnID() []byte                         { return u.ID }
func (u *passkeyUser) WebAuthnName() string                       { return u.Name }
func (u *passkeyUser) WebAuthnDisplayName() string                { return u.Name }
func (u *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.Creds }

type session struct {
	created, seen time.Time
	device        string
}

type authState struct {
	mu       sync.Mutex
	wa       *webauthn.WebAuthn
	path     string
	user     *passkeyUser
	pending  map[string]*webauthn.SessionData // ceremony id → challenge
	sessions map[string]*session              // sha256(token) → session
	pairCode string
	pairExp  time.Time
	pairLeft int
}

func newAuth(host, stateDir, owner string) (*authState, error) {
	wa, err := webauthn.New(&webauthn.Config{
		RPDisplayName: "mydevices",
		RPID:          host,
		RPOrigins:     []string{"https://" + host},
	})
	if err != nil {
		return nil, err
	}
	s := &authState{
		wa:       wa,
		path:     filepath.Join(stateDir, "passkeys.json"),
		pending:  map[string]*webauthn.SessionData{},
		sessions: map[string]*session{},
	}
	if b, err := os.ReadFile(s.path); err == nil {
		json.Unmarshal(b, &s.user)
	}
	if s.user == nil {
		s.user = &passkeyUser{ID: randBytes(32), Name: owner}
	}
	if s.user.RPID != host {
		// The hub's address changed (e.g. it was renamed): passkeys made for the old
		// address can't sign in here, so start fresh. Devices re-add theirs after the password.
		if len(s.user.Creds) > 0 {
			log.Printf("Hub address is now %s: passkeys for the old address were cleared", host)
		}
		s.user.RPID, s.user.Creds = host, nil
		s.save()
	}
	return s, nil
}

func (s *authState) save() {
	b, _ := json.MarshalIndent(s.user, "", "  ")
	os.WriteFile(s.path, b, 0o600)
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func tokenHash(t string) string {
	h := sha256.Sum256([]byte(t))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// valid reports whether the request carries a live session, refreshing its idle timer.
func (s *authState) valid(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := tokenHash(c.Value)
	ss := s.sessions[k]
	if ss == nil {
		return false
	}
	now := time.Now()
	if now.Sub(ss.created) > sessionMaxAge || now.Sub(ss.seen) > sessionIdle {
		delete(s.sessions, k)
		return false
	}
	ss.seen = now
	return true
}

func (s *authState) startSession(w http.ResponseWriter, device string) {
	tok := base64.RawURLEncoding.EncodeToString(randBytes(32))
	now := time.Now()
	s.mu.Lock()
	s.sessions[tokenHash(tok)] = &session{created: now, seen: now, device: device}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		MaxAge: int(sessionMaxAge.Seconds()), HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

// ---------- HTTP ----------

func (a *app) authRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", a.serveFile("login.html"))
	mux.HandleFunc("GET /auth/status", func(w http.ResponseWriter, r *http.Request) {
		a.auth2.mu.Lock()
		registered := len(a.auth2.user.Creds) > 0
		a.auth2.mu.Unlock()
		needPw := !a.passwordOK(r)
		writeJSON(w, map[string]any{"registered": registered, "authed": !needPw && a.auth2.valid(r), "needPassword": needPw, "codeNeeded": !a.sec.required(), "owner": a.owner})
	})
	mux.HandleFunc("POST /auth/password", a.passwordLogin)
	mux.HandleFunc("POST /auth/pair", a.pairRequest)
	mux.HandleFunc("POST /auth/register/begin", a.registerBegin)
	mux.HandleFunc("POST /auth/register/finish", a.registerFinish)
	mux.HandleFunc("POST /auth/login/begin", a.loginBegin)
	mux.HandleFunc("POST /auth/login/finish", a.loginFinish)
	mux.HandleFunc("POST /auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil {
			a.auth2.mu.Lock()
			delete(a.auth2.sessions, tokenHash(c.Value))
			a.auth2.mu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
		w.WriteHeader(http.StatusNoContent)
	})
}

// requireSession wraps everything except the login page and /auth/*.
func (a *app) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/login" || p == "/manifest.webmanifest" || strings.HasPrefix(p, "/icons/") || strings.HasPrefix(p, "/auth/") || strings.HasPrefix(p, "/.well-known/") || (a.passwordOK(r) && a.auth2.valid(r)) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		http.Error(w, "sign in required", http.StatusUnauthorized)
	})
}

// sameOrigin guards the state-changing auth endpoints against cross-site requests.
func (a *app) sameOrigin(r *http.Request) bool {
	return r.Header.Get("Origin") == "https://"+a.selfDNS
}

func (a *app) pairRequest(w http.ResponseWriter, r *http.Request) {
	if !a.sameOrigin(r) {
		http.Error(w, "bad origin", http.StatusForbidden)
		return
	}
	if !a.passwordOK(r) {
		http.Error(w, "enter the device password first", http.StatusForbidden)
		return
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	code := fmt.Sprintf("%06d", n.Int64())
	s := a.auth2
	s.mu.Lock()
	s.pairCode, s.pairExp, s.pairLeft = code, time.Now().Add(pairTTL), pairTries
	s.mu.Unlock()
	who := a.viewerName(r)
	log.Printf("🔑 Pairing code for %s: %s (valid 5 minutes)", who, code)
	notify("Pairing code: "+code, who+" wants to add a passkey")
	a.audit("pair-requested", who, "")
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) registerBegin(w http.ResponseWriter, r *http.Request) {
	if !a.sameOrigin(r) {
		http.Error(w, "bad origin", http.StatusForbidden)
		return
	}
	if !a.passwordOK(r) {
		http.Error(w, "enter the device password first", http.StatusForbidden)
		return
	}
	var body struct{ Code string }
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body)
	s := a.auth2
	// With a device password set, entering it (checked above) is enough to add a passkey,
	// so you can get in from a new phone without anyone at the Mac.
	ok := a.sec.required()
	if !ok {
		s.mu.Lock()
		ok = s.pairCode != "" && time.Now().Before(s.pairExp) && s.pairLeft > 0 && body.Code == s.pairCode
		if !ok && s.pairLeft > 0 {
			s.pairLeft--
		}
		if ok {
			s.pairCode = "" // single use
		}
		s.mu.Unlock()
	}
	if !ok {
		a.audit("pair-failed", a.viewerName(r), "")
		http.Error(w, "wrong or expired code", http.StatusForbidden)
		return
	}

	s.mu.Lock()
	var exclude []protocol.CredentialDescriptor
	for _, c := range s.user.Creds {
		exclude = append(exclude, c.Descriptor())
	}
	opts, sd, err := s.wa.BeginRegistration(s.user,
		webauthn.WithExclusions(exclude),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: protocol.VerificationRequired,
		}))
	id := base64.RawURLEncoding.EncodeToString(randBytes(16))
	if err == nil {
		s.pending["reg:"+id] = sd
	}
	s.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"id": id, "options": opts})
}

func (a *app) registerFinish(w http.ResponseWriter, r *http.Request) {
	if !a.sameOrigin(r) {
		http.Error(w, "bad origin", http.StatusForbidden)
		return
	}
	s := a.auth2
	s.mu.Lock()
	key := "reg:" + r.URL.Query().Get("id")
	sd := s.pending[key]
	delete(s.pending, key)
	s.mu.Unlock()
	if sd == nil {
		http.Error(w, "expired, try again", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	cred, err := s.wa.FinishRegistration(s.user, *sd, r)
	if err != nil {
		http.Error(w, "passkey rejected: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.user.Creds = append(s.user.Creds, *cred)
	s.save()
	s.mu.Unlock()
	who := a.viewerName(r)
	a.audit("passkey-added", who, "")
	notify("New passkey added", "from "+who)
	s.startSession(w, who)
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) loginBegin(w http.ResponseWriter, r *http.Request) {
	if !a.sameOrigin(r) {
		http.Error(w, "bad origin", http.StatusForbidden)
		return
	}
	if !a.passwordOK(r) {
		http.Error(w, "enter the device password first", http.StatusForbidden)
		return
	}
	s := a.auth2
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.user.Creds) == 0 {
		http.Error(w, "no passkeys yet", http.StatusConflict)
		return
	}
	opts, sd, err := s.wa.BeginLogin(s.user, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id := base64.RawURLEncoding.EncodeToString(randBytes(16))
	s.pending["login:"+id] = sd
	writeJSON(w, map[string]any{"id": id, "options": opts})
}

func (a *app) loginFinish(w http.ResponseWriter, r *http.Request) {
	if !a.sameOrigin(r) {
		http.Error(w, "bad origin", http.StatusForbidden)
		return
	}
	s := a.auth2
	s.mu.Lock()
	key := "login:" + r.URL.Query().Get("id")
	sd := s.pending[key]
	delete(s.pending, key)
	s.mu.Unlock()
	if sd == nil {
		http.Error(w, "expired, try again", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	who := a.viewerName(r)
	cred, err := s.wa.FinishLogin(s.user, *sd, r)
	if err != nil {
		a.audit("login-failed", who, err.Error())
		http.Error(w, "passkey check failed", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	for i := range s.user.Creds {
		if string(s.user.Creds[i].ID) == string(cred.ID) {
			s.user.Creds[i].Authenticator.SignCount = cred.Authenticator.SignCount
		}
	}
	s.save()
	s.mu.Unlock()
	a.audit("login", who, "")
	s.startSession(w, who)
	w.WriteHeader(http.StatusNoContent)
}

// ---------- audit log ----------

var auditMu sync.Mutex

func (a *app) audit(event, who, detail string) {
	auditMu.Lock()
	defer auditMu.Unlock()
	f, err := os.OpenFile(filepath.Join(a.opt.stateDir, "audit.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(map[string]string{"time": time.Now().Format(time.RFC3339), "event": event, "device": who, "detail": detail})
	f.Write(append(b, '\n'))
}
