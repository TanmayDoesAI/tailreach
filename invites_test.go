package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) (*inviteStore, *time.Time) {
	now := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	s := newInviteStore(t.TempDir())
	s.now = func() time.Time { return now }
	return s, &now
}

func TestInviteExpiry(t *testing.T) {
	s, now := testStore(t)
	if _, _, err := s.add("friend@example.com", 2*time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.get("Friend@Example.com "); !ok {
		t.Fatal("invite not found (logins compare without case)")
	}
	if _, ok := s.get("someone@example.com"); ok {
		t.Fatal("an uninvited login got in")
	}
	*now = now.Add(2 * time.Hour)
	if _, ok := s.get("friend@example.com"); ok {
		t.Fatal("expired invite still valid")
	}
	if gone := s.prune(); len(gone) != 1 || gone[0].Login != "friend@example.com" {
		t.Fatalf("prune = %v", gone)
	}
	if len(s.active()) != 0 {
		t.Fatal("pruned invite still listed")
	}
}

func TestInviteLimits(t *testing.T) {
	s, _ := testStore(t)
	for _, bad := range []string{"", "no-at-sign", "a b@example.com", "x/y@example.com"} {
		if _, _, err := s.add(bad, time.Hour, false); err == nil {
			t.Errorf("login %q accepted", bad)
		}
	}
	if _, _, err := s.add("a@example.com", inviteMax+time.Hour, false); err == nil {
		t.Error("an invite longer than 30 days was accepted")
	}
	_, inv, err := s.add("a@example.com", 0, false)
	if err != nil || inv.Expires.Sub(inv.Created) != inviteDefault {
		t.Errorf("default length: %v, %v", inv.Expires.Sub(inv.Created), err)
	}
}

func TestInviteCodeSingleUse(t *testing.T) {
	s, now := testStore(t)
	code, _, _ := s.add("friend@example.com", time.Hour, true)
	if len(normCode(code)) != codeLen {
		t.Fatalf("code %q", code)
	}
	if _, err := s.redeem("other@example.com", code); err != errNoInvite {
		t.Errorf("another login redeemed the code: %v", err)
	}
	typed := strings.ToLower(strings.ReplaceAll(code, "-", " "))
	inv, err := s.redeem("friend@example.com", typed)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if !inv.ViewOnly || inv.Used.IsZero() {
		t.Errorf("redeemed invite: %+v", inv)
	}
	if _, err := s.redeem("friend@example.com", code); err != errBadCode {
		t.Errorf("code worked twice: %v", err)
	}
	// The invite itself still lets them in after the code is spent.
	if _, ok := s.get("friend@example.com"); !ok {
		t.Error("invite gone after redeeming")
	}
	// A code can't be used after the invite ends.
	code2, _, _ := s.add("late@example.com", time.Hour, false)
	*now = now.Add(time.Hour)
	if _, err := s.redeem("late@example.com", code2); err != errNoInvite {
		t.Errorf("expired code: %v", err)
	}
}

func TestInviteCodeRateLimit(t *testing.T) {
	s, now := testStore(t)
	code, _, _ := s.add("friend@example.com", 24*time.Hour, false)
	for i := 1; i <= codeTries; i++ {
		_, err := s.redeem("friend@example.com", "AAAA-AAAA")
		if i < codeTries && err != errBadCode || i == codeTries && err != errJustLocked {
			t.Fatalf("try %d: %v", i, err)
		}
	}
	if _, err := s.redeem("friend@example.com", code); err != errCodeLocked {
		t.Fatalf("right code accepted while locked: %v", err)
	}
	*now = now.Add(codeTryWindow)
	if _, err := s.redeem("friend@example.com", code); err != nil {
		t.Fatalf("right code after the lockout: %v", err)
	}
}

func TestInviteRemoveAndPersist(t *testing.T) {
	dir := t.TempDir()
	s := newInviteStore(dir)
	code, _, _ := s.add("friend@example.com", time.Hour, true)
	s.add("other@example.com", time.Hour, false)
	fi, err := os.Stat(filepath.Join(dir, invitesFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("invites file: %v %v", fi.Mode(), err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, invitesFile))
	if strings.Contains(string(b), normCode(code)) || strings.Contains(string(b), code) {
		t.Fatal("the code itself was saved")
	}
	s2 := newInviteStore(dir)
	if inv, ok := s2.get("friend@example.com"); !ok || !inv.ViewOnly {
		t.Fatalf("not reloaded: %+v", inv)
	}
	if !s2.remove("FRIEND@example.com") || s2.remove("friend@example.com") {
		t.Fatal("remove")
	}
	if _, ok := s2.get("friend@example.com"); ok {
		t.Fatal("removed invite still valid")
	}
	if _, err := s2.redeem("friend@example.com", code); err != errNoInvite {
		t.Fatalf("removed invite's code: %v", err)
	}
	if _, ok := newInviteStore(dir).get("friend@example.com"); ok {
		t.Fatal("removal not saved")
	}
}

func TestReinviteReplacesCode(t *testing.T) {
	s, _ := testStore(t)
	old, _, _ := s.add("friend@example.com", time.Hour, false)
	code, _, _ := s.add("friend@example.com", time.Hour, true)
	if _, err := s.redeem("friend@example.com", old); err != errBadCode {
		t.Fatalf("old code still works: %v", err)
	}
	if inv, err := s.redeem("friend@example.com", code); err != nil || !inv.ViewOnly {
		t.Fatalf("new code: %+v %v", inv, err)
	}
}
