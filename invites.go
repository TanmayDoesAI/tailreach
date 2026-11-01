package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Guests: letting someone else (a friend, a colleague, family) see or use this Mac for a
// while. The owner invites a Tailscale login from the Mac itself (`tailreach invite`, or
// the settings window over the control socket); nothing about invites is reachable over
// the network except redeeming the one-time code.
//
//   - An invite names one Tailscale login, ends by itself (at most 30 days) and may be view only.
//   - auth() lets that login in while the invite lasts, and only to this Mac's own screen.
//   - The guest signs in with a one-time code (single use, rate limited), never with the
//     owner's device password or passkeys, and gets a short session of their own.
//   - View only is enforced on the server: forward drops input, uploads are refused.

const (
	inviteDefault = 24 * time.Hour
	inviteMax     = 30 * 24 * time.Hour
	guestSession  = 4 * time.Hour // a guest signs in again after this (with a new code)
	codeTries     = 5             // wrong codes allowed per login...
	codeTryWindow = 10 * time.Minute
	codeAlphabet  = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O, 1/I: read out over the phone
	codeLen       = 8                                  // 40 bits, and only 5 guesses per 10 minutes
	invitesFile   = "invites.json"
)

type invite struct {
	Login    string    `json:"login"`
	ViewOnly bool      `json:"viewOnly"`
	Created  time.Time `json:"created"`
	Expires  time.Time `json:"expires"`
	CodeHash string    `json:"codeHash,omitempty"` // sha256 of the code; empty once used
	Used     time.Time `json:"used,omitzero"`      // when the code was redeemed
}

type inviteStore struct {
	mu    sync.Mutex
	path  string
	list  map[string]*invite // by lower-cased login
	tries map[string][]time.Time
	now   func() time.Time // tests move the clock
}

var (
	errNoInvite   = errors.New("no invite for this account, or it has ended")
	errBadCode    = errors.New("wrong or used invite code")
	errCodeLocked = errors.New("too many wrong codes, try again in 10 minutes")
	errJustLocked = errors.New("too many wrong codes, try again in 10 minutes") // this try locked it: worth telling the Mac once
)

func newInviteStore(dir string) *inviteStore {
	s := &inviteStore{path: filepath.Join(dir, invitesFile), list: map[string]*invite{}, tries: map[string][]time.Time{}, now: time.Now}
	if b, err := os.ReadFile(s.path); err == nil {
		var list []*invite
		if json.Unmarshal(b, &list) == nil {
			for _, inv := range list {
				if inv.Login != "" {
					s.list[loginKey(inv.Login)] = inv
				}
			}
		}
	}
	return s
}

// Tailscale logins are e-mail-like; compare them without regard to case or stray spaces.
func loginKey(l string) string { return strings.ToLower(strings.TrimSpace(l)) }

func (s *inviteStore) saveLocked() {
	list := make([]*invite, 0, len(s.list))
	for _, inv := range s.list {
		list = append(list, inv)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Expires.Before(list[j].Expires) })
	b, _ := json.MarshalIndent(list, "", "  ")
	os.WriteFile(s.path, b, 0o600)
}

func codeHash(code string) string {
	h := sha256.Sum256([]byte(normCode(code)))
	return hex.EncodeToString(h[:])
}

// normCode accepts what people type: any case, with spaces or a dash in the middle.
func normCode(c string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(c) {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func newCode() string {
	b := make([]byte, codeLen)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(codeAlphabet))))
		b[i] = codeAlphabet[n.Int64()]
	}
	return string(b[:4]) + "-" + string(b[4:])
}

// add invites a login (replacing any earlier invite for it, and its code) and returns
// the new one-time code. The code itself is never stored, only its hash.
func (s *inviteStore) add(login string, d time.Duration, viewOnly bool) (string, invite, error) {
	login = strings.TrimSpace(login)
	if login == "" || len(login) > 254 || strings.ContainsAny(login, " \t\r\n/") || !strings.Contains(login, "@") {
		return "", invite{}, errors.New("give their Tailscale login, e.g. name@example.com or name@github")
	}
	if d <= 0 {
		d = inviteDefault
	}
	if d > inviteMax {
		return "", invite{}, errors.New("an invite lasts at most 30 days (720 hours)")
	}
	code := newCode()
	now := s.now()
	inv := &invite{Login: login, ViewOnly: viewOnly, Created: now, Expires: now.Add(d), CodeHash: codeHash(code)}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list[loginKey(login)] = inv
	delete(s.tries, loginKey(login))
	s.saveLocked()
	return code, *inv, nil
}

func (s *inviteStore) remove(login string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := loginKey(login)
	if _, ok := s.list[k]; !ok {
		return false
	}
	delete(s.list, k)
	delete(s.tries, k)
	s.saveLocked()
	return true
}

// get returns the login's invite if it hasn't ended. Denies by default: no invite, no entry.
func (s *inviteStore) get(login string) (invite, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv := s.list[loginKey(login)]
	if inv == nil || !s.now().Before(inv.Expires) {
		return invite{}, false
	}
	return *inv, true
}

// redeem checks a guest's code: right login, unexpired, unused, constant-time compare,
// and at most codeTries wrong codes per login in codeTryWindow. A right code is spent.
func (s *inviteStore) redeem(login, code string) (invite, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, now := loginKey(login), s.now()
	recent := s.tries[k][:0]
	for _, t := range s.tries[k] {
		if now.Sub(t) < codeTryWindow {
			recent = append(recent, t)
		}
	}
	s.tries[k] = recent
	if len(recent) >= codeTries {
		return invite{}, errCodeLocked
	}
	inv := s.list[k]
	if inv == nil || !now.Before(inv.Expires) {
		return invite{}, errNoInvite
	}
	// Hash first so the comparison is of equal-length values, then compare in constant time.
	ok := inv.CodeHash != "" && subtle.ConstantTimeCompare([]byte(codeHash(code)), []byte(inv.CodeHash)) == 1
	if !ok {
		s.tries[k] = append(s.tries[k], now)
		if len(s.tries[k]) >= codeTries {
			return invite{}, errJustLocked
		}
		return invite{}, errBadCode
	}
	inv.CodeHash, inv.Used = "", now // single use
	delete(s.tries, k)
	s.saveLocked()
	return *inv, nil
}

// prune drops invites that have ended and returns them, so the caller can log them and
// end anything their guests still have open.
func (s *inviteStore) prune() []invite {
	s.mu.Lock()
	defer s.mu.Unlock()
	var gone []invite
	now := s.now()
	for k, inv := range s.list {
		if !now.Before(inv.Expires) {
			gone = append(gone, *inv)
			delete(s.list, k)
			delete(s.tries, k)
		}
	}
	if len(gone) > 0 {
		s.saveLocked()
	}
	return gone
}

// active lists the invites still running, soonest to end first, without their codes.
func (s *inviteStore) active() []invite {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := []invite{}
	now := s.now()
	for _, inv := range s.list {
		if now.Before(inv.Expires) {
			c := *inv
			c.CodeHash = ""
			list = append(list, c)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Expires.Before(list[j].Expires) })
	return list
}
