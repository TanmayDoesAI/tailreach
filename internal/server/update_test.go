package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.2.0", "0.2.0", 0},
		{"v0.2.0", "0.2.0", 0},
		{"0.2.1", "0.2.0", 1},
		{"0.10.0", "0.9.9", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.1.9", "0.2.0", -1},
		{"0.3.0-rc1", "0.2.0", -1}, // a pre-release never counts as newer
		{"junk", "0.2.0", -1},
		{"0.2", "0.2.0", -1},
		{"01.2.3", "0.2.0", -1},
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseRelease(t *testing.T) {
	for _, c := range []struct {
		body string
		want *updateInfo
	}{
		{`{"tag_name":"v0.3.0","html_url":"https://github.com/TanmayDoesAI/tailreach/releases/tag/v0.3.0"}`,
			&updateInfo{"0.3.0", "https://github.com/TanmayDoesAI/tailreach/releases/tag/v0.3.0"}},
		{`{"tag_name":"0.3.1","html_url":"javascript:alert(1)"}`, // only ever a github.com link
			&updateInfo{"0.3.1", "https://github.com/TanmayDoesAI/tailreach/releases/tag/0.3.1"}},
		{`{"tag_name":"v0.4.0","draft":true}`, nil},
		{`{"tag_name":"v0.4.0","prerelease":true}`, nil},
		{`{"tag_name":"v0.4.0-beta"}`, nil},
		{`{"tag_name":"nightly"}`, nil},
	} {
		got, err := parseRelease(strings.NewReader(c.body))
		if err != nil {
			t.Fatalf("%s: %v", c.body, err)
		}
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: got %+v, want %+v", c.body, got, c.want)
		}
	}
	if _, err := parseRelease(strings.NewReader("<html>")); err == nil {
		t.Error("garbage parsed")
	}
}

func TestUpdateCheckETag(t *testing.T) {
	tag, hits, conditional := "v0.9.0", 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if !strings.HasPrefix(r.UserAgent(), "tailreach/") {
			t.Errorf("User-Agent %q", r.UserAgent())
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("the update check sent credentials")
		}
		etag := `"` + tag + `"`
		if r.Header.Get("If-None-Match") == etag {
			conditional++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Write([]byte(`{"tag_name":"` + tag + `","html_url":"https://github.com/TanmayDoesAI/tailreach/releases/tag/` + tag + `"}`))
	}))
	defer srv.Close()

	u := newUpdateChecker("0.2.0")
	u.url = srv.URL
	ctx := context.Background()
	if u.available() != nil {
		t.Fatal("an update before any check")
	}
	if err := u.check(ctx); err != nil {
		t.Fatal(err)
	}
	if up := u.available(); up == nil || up.Version != "0.9.0" {
		t.Fatalf("after first check: %+v", up)
	}
	// Same ETag: a 304, and what was known stays.
	if err := u.check(ctx); err != nil {
		t.Fatal(err)
	}
	if conditional != 1 || u.available() == nil {
		t.Fatalf("304 handling: conditional=%d available=%+v", conditional, u.available())
	}
	// A new release: the old ETag no longer matches.
	tag = "v1.0.0"
	if err := u.check(ctx); err != nil {
		t.Fatal(err)
	}
	if up := u.available(); up == nil || up.Version != "1.0.0" || hits != 3 {
		t.Fatalf("after new release: %+v (hits %d)", up, hits)
	}
	// Running the latest (or newer): nothing to show.
	u.current = "1.0.0"
	if u.available() != nil {
		t.Error("an update to the version already running")
	}
}

func TestUpdateCheckOptOut(t *testing.T) {
	a := &app{sec: &security{}, updates: newUpdateChecker("0.2.0")}
	a.updates.latest = &updateInfo{"9.0.0", "https://github.com/x"}
	if a.updateNotice() == nil {
		t.Fatal("no notice with checks on")
	}
	a.sec.cfg.NoUpdateCheck = true
	if a.updateCheckOn() || a.updateNotice() != nil {
		t.Error("settings opt-out ignored")
	}
	a.sec.cfg.NoUpdateCheck = false
	a.opt.noUpdateCheck = true
	if a.updateCheckOn() || a.updateNotice() != nil {
		t.Error("--no-update-check ignored")
	}
}
