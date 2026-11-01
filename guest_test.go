package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestViewOnlyForward(t *testing.T) {
	// Everything a viewer can send that reaches the Mac must be dropped for a view-only guest.
	for cmd := range allowedCmds {
		if cmd == "start" || cmd == "kf" || cmd == "resend" {
			continue
		}
		if mayForward(cmd, true) {
			t.Errorf("view only lets %q through", cmd)
		}
	}
	for _, cmd := range []string{"text", "clip-set", "clip-get", "extend", "unextend", "diag", "displays", "anything-new", ""} {
		if mayForward(cmd, true) {
			t.Errorf("view only lets %q through", cmd)
		}
	}
	for _, cmd := range []string{"kf", "resend", "ping", "spong", "bye", "vstats"} {
		if !mayForward(cmd, true) {
			t.Errorf("view only blocks %q, which keeps the stream going", cmd)
		}
	}
	for cmd := range allowedCmds {
		if !mayForward(cmd, false) {
			t.Errorf("full control blocks %q", cmd)
		}
	}
}

func TestGuestCantTakeFromOwner(t *testing.T) {
	kicked := ""
	_, rel := claimStream("owner-phone", false, false, func(by string) { kicked = by })
	defer rel()
	if by, _ := claimStream("guest a (laptop)", true, true, func(string) {}); by != "owner-phone" || kicked != "" {
		t.Fatalf("guest took over the owner: busy=%q kicked=%q", by, kicked)
	}
	by, rel2 := claimStream("owner-laptop", false, true, func(string) {})
	if by != "" || kicked != "owner-laptop" {
		t.Fatalf("owner takeover: busy=%q kicked=%q", by, kicked)
	}
	rel2()
}

func TestOwnerGuestSessionsSeparate(t *testing.T) {
	s := &authState{sessions: map[string]*session{}}
	cookieOf := func(w *httptest.ResponseRecorder) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		for _, c := range w.Result().Cookies() {
			r.AddCookie(c)
		}
		return r
	}
	w := httptest.NewRecorder()
	s.startSessionFor(w, "laptop", "friend@example.com", time.Now().Add(time.Hour))
	gr := cookieOf(w)
	if s.valid(gr) {
		t.Error("a guest's session counts as the owner's")
	}
	if !s.validFor(gr, "friend@example.com") || s.validFor(gr, "other@example.com") {
		t.Error("guest session not bound to its login")
	}
	w = httptest.NewRecorder()
	s.startSession(w, "phone")
	or := cookieOf(w)
	if !s.valid(or) || s.validFor(or, "friend@example.com") {
		t.Error("owner session")
	}
	s.dropGuestSessions("Friend@example.com")
	if s.validFor(gr, "friend@example.com") || !s.valid(or) {
		t.Error("dropGuestSessions")
	}
	w = httptest.NewRecorder()
	s.startSessionFor(w, "laptop", "friend@example.com", time.Now().Add(-time.Second))
	if s.validFor(cookieOf(w), "friend@example.com") {
		t.Error("a session outlived its expiry")
	}
}

func TestRequireSessionForGuests(t *testing.T) {
	a := &app{auth2: &authState{sessions: map[string]*session{}}, sec: &security{}}
	h := a.requireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) }))
	g := &invite{Login: "friend@example.com", Expires: time.Now().Add(time.Hour)}
	owner := httptest.NewRecorder()
	a.auth2.startSession(owner, "phone")
	guest := httptest.NewRecorder()
	a.auth2.startSessionFor(guest, "laptop", g.Login, g.Expires)
	req := func(path string, as *invite, w *httptest.ResponseRecorder) int {
		r := httptest.NewRequest("POST", path, nil)
		if w != nil {
			for _, c := range w.Result().Cookies() {
				r.AddCookie(c)
			}
		}
		if as != nil {
			r = r.WithContext(context.WithValue(r.Context(), guestKey{}, as))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	for _, c := range []struct {
		path string
		as   *invite
		w    *httptest.ResponseRecorder
		want int
	}{
		{"/auth/invite", g, nil, 299},
		{"/auth/status", g, nil, 299},
		{"/auth/password", g, nil, 403},
		{"/auth/register/begin", g, nil, 403},
		{"/auth/login/begin", g, nil, 403},
		{"/auth/pair", g, nil, 403},
		{"/mac/stream", g, nil, 401},
		{"/mac/stream", g, owner, 401}, // the owner's session is no use to a guest
		{"/mac/stream", g, guest, 299},
		{"/mac/stream", nil, guest, 401}, // nor a guest's to anyone else
		{"/mac/stream", nil, owner, 299},
	} {
		if got := req(c.path, c.as, c.w); got != c.want {
			t.Errorf("%s (guest %v): %d, want %d", c.path, c.as != nil, got, c.want)
		}
	}
}
